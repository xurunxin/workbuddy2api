package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func managerFixture(t *testing.T) (*configManager, string) {
	t.Helper()
	dir := t.TempDir()
	c := Default()
	c.StateFile = filepath.Join(dir, "state.json")
	c.APIKey = "test-private-key"
	c.Admin.Password = "test-private-admin-password"
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(base, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := newConfigManager(c)
	if err != nil {
		t.Fatal(err)
	}
	return m, base
}
func TestManagedConfigSaveRestartAndSecrets(t *testing.T) {
	m, path := managerFixture(t)
	if err := m.save(json.RawMessage(`{"server":{"max_body_mb":16},"schedule":{"checkin_enabled":false}}`)); err != nil {
		t.Fatal(err)
	}
	if m.active.Server.MaxBodyMB != 8 {
		t.Fatal("mutated live config")
	}
	raw, _ := json.Marshal(m.read())
	for _, secret := range []string{"test-private-key", "replacement-key", "test-private-admin-password"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("secret exposed")
		}
	}
	if !strings.Contains(string(raw), `"pending_restart":true`) {
		t.Fatal("missing restart status")
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxBodyMB != 16 || c.Schedule.CheckinEnabled || c.APIKey != "test-private-key" {
		t.Fatal("saved config not restored")
	}
	newManager, err := newConfigManager(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(newManager.read())
	if !strings.Contains(string(raw), `"pending_restart":false`) {
		t.Fatal(string(raw))
	}
}
func TestManagedConfigInvalidPreservesFile(t *testing.T) {
	m, _ := managerFixture(t)
	if err := m.save(json.RawMessage(`{"server":{"max_body_mb":16}}`)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(m.path)
	for _, raw := range []string{`null`, `{"listen":":1"}`, `{"admin":{"password":"evil"}}`, `{"pool":{"unknown":1}}`, `{"pool":{"max_in_flight":-1}}`, `{"server":{"max_body_mb":0}}`, `{"session_sticky":{"gc_interval":"-1m"}}`, `{"schedule":{"checkin_hours":[25]}}`, `{"upstream":{"timeout_seconds":"oops"}}`, `{"prompt":{"mode":"unknown"}}`, `{"features":{"sanitize_blacklist_fingerprints":null}}`} {
		if err := m.save(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	after, _ := os.ReadFile(m.path)
	if string(before) != string(after) {
		t.Fatal("bad edit modified persisted config")
	}
}
func TestManagedConfigEnvironmentWins(t *testing.T) {
	m, path := managerFixture(t)
	t.Setenv("WB2A_MAX_BODY_MB", "24")
	t.Setenv("WB2A_API_KEY", "env-secret")
	if err := m.save(json.RawMessage(`{"server":{"max_body_mb":16}}`)); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.Server.MaxBodyMB != 24 || c.APIKey != "env-secret" {
		t.Fatal("env lost precedence")
	}
	raw, _ := json.Marshal(m.read())
	if strings.Contains(string(raw), "env-secret") || !strings.Contains(string(raw), "WB2A_API_KEY") {
		t.Fatal("bad env disclosure")
	}
}

func TestManagedLegacyKeyLoadButRejectNewEdits(t *testing.T) {
	m, path := managerFixture(t)
	if err := os.WriteFile(m.path, []byte(`{"api_key":"legacy-overlay-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.APIKey != "legacy-overlay-key" {
		t.Fatal("legacy key must remain loadable for migration", err)
	}
	for _, patch := range []string{`{"api_key":"new-key"}`, `{"api_key":""}`} {
		if err := m.save(json.RawMessage(patch)); err == nil {
			t.Fatal("accepted obsolete key edit")
		}
	}
	raw, _ := os.ReadFile(m.path)
	if string(raw) != `{"api_key":"legacy-overlay-key"}` {
		t.Fatal("rejected patch modified file")
	}
}
