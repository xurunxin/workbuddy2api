// Package accesskey persists gateway API keys without ever storing their
// plaintext values. It is deliberately independent from the HTTP layer so the
// same Store can be used by startup migration and live administration.
package accesskey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	fileVersion = 1
	// MaxKeys bounds the durable key set and lets HTTP callers describe a
	// capacity conflict without duplicating a storage policy constant.
	MaxKeys = 1000
)

// ErrNotFound is returned by Revoke when the requested key id does not exist.
// Callers can map it to their public HTTP not-found response without matching
// an error string.
var (
	ErrNotFound     = errors.New("access key not found")
	ErrInvalidName  = errors.New("invalid access key name")
	ErrLimitReached = errors.New("access key record limit reached")
	ErrPersistence  = errors.New("access key persistence failed")
)

// View is the safe, public representation of an API key. Hashes and plaintext
// values are intentionally absent.
type View struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Source    string     `json:"source"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	Status    string     `json:"status"`
}

// Store is safe for concurrent use. Mutations first persist a candidate state,
// then publish it to readers, so a failed disk write never authorizes a key in
// memory that will disappear after a restart.
type Store struct {
	mu   sync.RWMutex
	path string
	data diskState
}

type diskState struct {
	Version                int       `json:"version"`
	AuthenticationRequired bool      `json:"authentication_required"`
	Keys                   []diskKey `json:"keys"`
}

type diskKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Source    string     `json:"source"`
	Hash      string     `json:"hash"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Open loads path. When the file does not yet exist, a non-empty legacyKey is
// migrated once into a hashed record. Existing files always win over legacyKey;
// this prevents a revoked legacy credential from being resurrected on restart.
func Open(path, legacyKey string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("access key store path is required")
	}

	raw, err := os.ReadFile(path)
	if err == nil {
		state, err := decodeState(raw)
		if err != nil {
			return nil, fmt.Errorf("load access key store: %w", err)
		}
		return &Store{path: path, data: state}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read access key store: %w", err)
	}

	state := diskState{Version: fileVersion}
	if legacyKey != "" {
		key, err := makeKey("原配置密钥", "legacy", legacyKey)
		if err != nil {
			return nil, fmt.Errorf("migrate legacy access key: %w", err)
		}
		state.AuthenticationRequired = true
		state.Keys = []diskKey{key}
		if err := writeAtomically(path, state); err != nil {
			return nil, fmt.Errorf("persist legacy access key migration: %w", err)
		}
	}
	return &Store{path: path, data: state}, nil
}

// List returns safe snapshots of all records in creation order.
func (s *Store) List() []View {
	s.mu.RLock()
	defer s.mu.RUnlock()

	views := make([]View, len(s.data.Keys))
	for i, key := range s.data.Keys {
		views[i] = key.view()
	}
	return views
}

// Required reports whether the store has permanently entered API-key
// authentication mode. An empty, never-initialized store remains compatible
// with historical anonymous operation.
func (s *Store) Required() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.AuthenticationRequired
}

// Validate accepts every request only while the store remains in its initial
// anonymous compatibility state. Once authentication is enabled it compares
// SHA-256 values in constant time and accepts active keys only.
func (s *Store) Validate(key string) bool {
	_, valid := s.Identify(key)
	return valid
}

// Identify returns a safe durable ID along with the authentication decision.
func (s *Store) Identify(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.data.AuthenticationRequired {
		return "anonymous", true
	}
	sum := sha256.Sum256([]byte(key))
	encoded := hex.EncodeToString(sum[:])
	valid := 0
	id := ""
	for _, item := range s.data.Keys {
		if item.RevokedAt == nil {
			match := subtle.ConstantTimeCompare([]byte(encoded), []byte(item.Hash))
			valid |= match
			if match == 1 {
				id = item.ID
			}
		}
	}
	return id, valid == 1
}

// Create generates a new API key. The plaintext result is returned only from
// this call; the disk state and every View contain only the hash and prefix.
func (s *Store) Create(name string) (View, string, error) {
	name = strings.TrimSpace(name)
	if err := validateName(name); err != nil {
		return View{}, "", fmt.Errorf("%w: %v", ErrInvalidName, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Keys) >= MaxKeys {
		return View{}, "", fmt.Errorf("%w (%d)", ErrLimitReached, MaxKeys)
	}

	secret, err := randomSecret()
	if err != nil {
		return View{}, "", fmt.Errorf("generate access key: %w", err)
	}
	item, err := makeKey(name, "created", secret)
	if err != nil {
		return View{}, "", fmt.Errorf("create access key: %w", err)
	}
	candidate := cloneState(s.data)
	candidate.AuthenticationRequired = true
	candidate.Keys = append(candidate.Keys, item)
	if err := writeAtomically(s.path, candidate); err != nil {
		return View{}, "", ErrPersistence
	}
	s.data = candidate
	return item.view(), secret, nil
}

// Revoke permanently disables the key identified by id. Revoking an already
// revoked record is successful and does not create needless writes.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := -1
	for i := range s.data.Keys {
		if s.data.Keys[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return ErrNotFound
	}
	if s.data.Keys[index].RevokedAt != nil {
		return nil
	}

	candidate := cloneState(s.data)
	now := time.Now().UTC()
	candidate.Keys[index].RevokedAt = &now
	if err := writeAtomically(s.path, candidate); err != nil {
		return ErrPersistence
	}
	s.data = candidate
	return nil
}

func (key diskKey) view() View {
	status := "active"
	if key.RevokedAt != nil {
		status = "revoked"
	}
	return View{
		ID: key.ID, Name: key.Name, Prefix: key.Prefix, Source: key.Source,
		CreatedAt: key.CreatedAt, RevokedAt: cloneTime(key.RevokedAt), Status: status,
	}
}

func makeKey(name, source, secret string) (diskKey, error) {
	id, err := randomID()
	if err != nil {
		return diskKey{}, err
	}
	sum := sha256.Sum256([]byte(secret))
	return diskKey{
		ID: id, Name: name, Prefix: maskedPrefix(secret), Source: source,
		Hash: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC(),
	}, nil
}

func randomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func randomSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", err
	}
	return "wb2a_" + hex.EncodeToString(bytes), nil
}

func maskedPrefix(secret string) string {
	const (
		visible      = 12
		shortVisible = 4
	)
	runes := []rune(secret)
	if len(runes) <= shortVisible {
		// A short legacy key must not be fully recoverable from either disk or
		// the public View. A fixed mask also avoids revealing its length.
		return "••••"
	}
	if len(runes) <= visible {
		return string(runes[:shortVisible]) + "…"
	}
	return string(runes[:visible]) + "…"
}

func decodeState(raw []byte) (diskState, error) {
	var state diskState
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return diskState{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return diskState{}, errors.New("invalid JSON: multiple values")
	}
	if err := validateState(state); err != nil {
		return diskState{}, err
	}
	return state, nil
}

func validateState(state diskState) error {
	if state.Version != fileVersion {
		return fmt.Errorf("unsupported access key store version %d", state.Version)
	}
	if len(state.Keys) > MaxKeys {
		return fmt.Errorf("access key store exceeds record limit %d", MaxKeys)
	}
	if len(state.Keys) > 0 && !state.AuthenticationRequired {
		return errors.New("access key store has keys but authentication is disabled")
	}
	ids := make(map[string]struct{}, len(state.Keys))
	hashes := make(map[string]struct{}, len(state.Keys))
	for _, key := range state.Keys {
		if err := validateDiskKey(key); err != nil {
			return err
		}
		if _, found := ids[key.ID]; found {
			return fmt.Errorf("duplicate access key id %q", key.ID)
		}
		ids[key.ID] = struct{}{}
		if _, found := hashes[key.Hash]; found {
			return errors.New("duplicate access key hash")
		}
		hashes[key.Hash] = struct{}{}
	}
	return nil
}

func validateDiskKey(key diskKey) error {
	decodedID, err := base64.RawURLEncoding.DecodeString(key.ID)
	if err != nil || len(decodedID) != 16 {
		return errors.New("invalid access key id")
	}
	if err := validateName(key.Name); err != nil {
		return fmt.Errorf("invalid access key name: %w", err)
	}
	if key.Source != "legacy" && key.Source != "created" {
		return errors.New("invalid access key source")
	}
	if key.Prefix == "" || utf8.RuneCountInString(key.Prefix) > 80 || strings.ContainsAny(key.Prefix, "\r\n") {
		return errors.New("invalid access key prefix")
	}
	if len(key.Hash) != sha256.Size*2 || strings.ToLower(key.Hash) != key.Hash {
		return errors.New("invalid access key hash")
	}
	decodedHash, err := hex.DecodeString(key.Hash)
	if err != nil || len(decodedHash) != sha256.Size {
		return errors.New("invalid access key hash")
	}
	if key.CreatedAt.IsZero() {
		return errors.New("invalid access key creation time")
	}
	if key.RevokedAt != nil && key.RevokedAt.Before(key.CreatedAt) {
		return errors.New("invalid access key revocation time")
	}
	return nil
}

func validateName(name string) error {
	if name == "" || name != strings.TrimSpace(name) || utf8.RuneCountInString(name) > 80 {
		return errors.New("access key name must contain 1 to 80 non-space-trimmed characters")
	}
	return nil
}

func cloneState(state diskState) diskState {
	result := diskState{Version: state.Version, AuthenticationRequired: state.AuthenticationRequired}
	result.Keys = make([]diskKey, len(state.Keys))
	copy(result.Keys, state.Keys)
	for i := range result.Keys {
		result.Keys[i].RevokedAt = cloneTime(result.Keys[i].RevokedAt)
	}
	return result
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func writeAtomically(path string, state diskState) (err error) {
	if err := validateState(state); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	temporary, err := os.CreateTemp(directory, ".access-keys-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}
