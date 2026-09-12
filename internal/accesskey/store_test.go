package accesskey

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenMigratesLegacyWithoutPersistingPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "access-keys.json")
	legacy := "old-config-secret-value"
	store, err := Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Required() || !store.Validate(legacy) || store.Validate("wrong") {
		t.Fatal("legacy migration did not enforce and validate the legacy key")
	}
	views := store.List()
	if len(views) != 1 || views[0].Name != "原配置密钥" || views[0].Source != "legacy" || views[0].Status != "active" {
		t.Fatalf("unexpected migrated view: %#v", views)
	}
	if strings.Contains(views[0].Prefix, legacy) || !strings.HasSuffix(views[0].Prefix, "…") {
		t.Fatalf("legacy prefix is not masked: %q", views[0].Prefix)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), legacy) || !strings.Contains(string(raw), `"hash"`) {
		t.Fatalf("disk state leaked legacy plaintext or omitted its hash: %s", raw)
	}
	viewJSON, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(viewJSON), legacy) || strings.Contains(string(viewJSON), `"hash"`) {
		t.Fatalf("public view leaked secret material: %s", viewJSON)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("store permissions too broad: %o", info.Mode().Perm())
	}
}

func TestShortLegacyKeyDoesNotLeakThroughPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	legacy := "q!7"
	store, err := Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	view := store.List()[0]
	if view.Prefix != "••••" || strings.Contains(view.Prefix, legacy) {
		t.Fatalf("short legacy prefix leaked the complete key: %q", view.Prefix)
	}
	viewJSON, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(viewJSON), legacy) {
		t.Fatalf("short legacy key leaked from public view: %s", viewJSON)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), legacy) {
		t.Fatalf("short legacy key leaked from disk state: %s", raw)
	}
}

func TestExistingStoreNeverResurrectsLegacyKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := Open(path, "old-key")
	if err != nil {
		t.Fatal(err)
	}
	id := store.List()[0].ID
	if err := store.Revoke(id); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path, "old-key")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Validate("old-key") || !restarted.Required() || restarted.List()[0].Status != "revoked" {
		t.Fatal("a configured legacy key was resurrected after restart")
	}
}

func TestMultipleKeysAndRevocationPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	first, firstSecret, err := store.Create("primary")
	if err != nil {
		t.Fatal(err)
	}
	second, secondSecret, err := store.Create("CI key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(firstSecret, "wb2a_") || len(strings.TrimPrefix(firstSecret, "wb2a_")) != 64 {
		t.Fatalf("unexpected generated key format: %q", firstSecret)
	}
	if !store.Required() || !store.Validate(firstSecret) || !store.Validate(secondSecret) || len(store.List()) != 2 {
		t.Fatal("multiple active keys were not accepted")
	}
	if err := store.Revoke(first.ID); err != nil {
		t.Fatal(err)
	}
	if store.Validate(firstSecret) || !store.Validate(secondSecret) {
		t.Fatal("revocation did not become effective immediately")
	}
	if err := store.Revoke(first.ID); err != nil {
		t.Fatalf("repeat revocation should be idempotent: %v", err)
	}
	if err := store.Revoke("does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id error = %v, want ErrNotFound", err)
	}
	restarted, err := Open(path, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Validate(firstSecret) || !restarted.Validate(secondSecret) || restarted.List()[0].Status != "revoked" || second.Status != "active" {
		t.Fatal("revocation state did not survive restart")
	}
}

func TestAuthenticationDoesNotReturnToAnonymousMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := Open(path, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(store.List()[0].ID); err != nil {
		t.Fatal(err)
	}
	if !store.Required() || store.Validate("anything") {
		t.Fatal("a store with only revoked keys must still require authentication")
	}
}

func TestEmptyLegacyEnablesAuthenticationOnFirstCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if store.Required() || !store.Validate("arbitrary") {
		t.Fatal("empty first-run store should retain anonymous compatibility")
	}
	_, secret, err := store.Create("first")
	if err != nil {
		t.Fatal(err)
	}
	if !store.Required() || !store.Validate(secret) || store.Validate("arbitrary") {
		t.Fatal("first key did not permanently enable authentication")
	}
}

func TestCorruptStoreFailsClosedWithoutRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	corrupt := []byte(`{"version":1,"authentication_required":true,"keys":`) // deliberately truncated
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "would-be-legacy"); err == nil {
		t.Fatal("corrupt store unexpectedly opened")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(corrupt) {
		t.Fatalf("corrupt store was rewritten: %q", after)
	}
}

func TestFailedWriteDoesNotPublishNewKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "blocked", "keys.json")
	store, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	// Open with an empty legacy key deliberately does not create a file. Turn
	// its would-be parent into a regular file so Create cannot make the parent.
	if err := os.WriteFile(filepath.Join(root, "blocked"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create("will fail"); !errors.Is(err, ErrPersistence) || strings.Contains(err.Error(), root) {
		t.Fatalf("Create failure should be generic persistence error, got %v", err)
	}
	if store.Required() || len(store.List()) != 0 || !store.Validate("anonymous") {
		t.Fatal("failed persistence changed the in-memory authorization state")
	}
}

func TestFailedRevocationWriteLeavesKeyActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	view, secret, err := store.Create("active")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(view.ID); !errors.Is(err, ErrPersistence) || strings.Contains(err.Error(), path) {
		t.Fatalf("Revoke failure should be generic persistence error, got %v", err)
	}
	if !store.Validate(secret) || store.List()[0].Status != "active" {
		t.Fatal("failed revocation persistence changed the in-memory authorization state")
	}
}

func TestConcurrentValidateCreateAndRevoke(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := store.Create("concurrent")
	if err != nil {
		t.Fatal(err)
	}
	id := store.List()[0].ID
	var group sync.WaitGroup
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for n := 0; n < 100; n++ {
				_ = store.Validate(secret)
				_ = store.List()
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		if err := store.Revoke(id); err != nil {
			t.Errorf("revoke: %v", err)
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		for n := 0; n < 5; n++ {
			view, _, err := store.Create("parallel key")
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			if err := store.Revoke(view.ID); err != nil {
				t.Errorf("revoke created key: %v", err)
				return
			}
		}
	}()
	group.Wait()
	if store.Validate(secret) {
		t.Fatal("concurrent revocation was not observed")
	}
}

func TestCreateNameValidation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "   ", strings.Repeat("x", 81)} {
		if _, _, err := store.Create(name); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("Create(%q) unexpectedly succeeded", name)
		}
	}
	view, _, err := store.Create("  usable name  ")
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != "usable name" {
		t.Fatalf("name was not trimmed: %q", view.Name)
	}
}

func TestCreateLimitErrorIsMappable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.data.Keys = make([]diskKey, MaxKeys)
	store.mu.Unlock()
	if _, _, err := store.Create("over limit"); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("Create limit error = %v, want ErrLimitReached", err)
	}
}

func TestRejectsInvalidPersistedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	state := diskState{Version: fileVersion, AuthenticationRequired: false, Keys: []diskKey{{
		ID: "not-valid", Name: "key", Prefix: "x…", Source: "created", Hash: strings.Repeat("0", 64),
		CreatedAt: time.Now(),
	}}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, ""); err == nil {
		t.Fatal("invalid persisted state unexpectedly opened")
	}
}
