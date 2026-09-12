package usage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Totals struct {
	Requests     int64 `json:"requests"`
	Failures     int64 `json:"failures"`
	Input        int64 `json:"input_tokens"`
	Output       int64 `json:"output_tokens"`
	Cached       int64 `json:"cached_tokens"`
	CacheMissing int64 `json:"cache_missing_usage"`
	Missing      int64 `json:"missing_usage"`
}

type Row struct {
	Day   string `json:"day"`
	KeyID string `json:"key_id"`
	Totals
}

type Credit struct {
	UID       string    `json:"uid"`
	Remain    int64     `json:"remain"`
	Used      *int64    `json:"used"`
	CheckedAt time.Time `json:"checked_at"`
}

type diskState struct {
	Rows    []Row    `json:"rows"`
	Credits []Credit `json:"credits"`
}

type Store struct {
	mu        sync.Mutex
	path      string
	rows      []Row
	credits   []Credit
	lastError bool
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, rows: []Row{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var state diskState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	s.rows, s.credits = state.Rows, state.Credits
	// Older files did not collect cache usage; their requests are unknown, not misses.
	var legacy struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, err
	}
	for i, row := range legacy.Rows {
		if _, ok := row["cache_missing_usage"]; !ok {
			s.rows[i].CacheMissing = s.rows[i].Requests
		}
	}
	return s, nil
}

func (s *Store) SetCredit(uid string, remain int64, used *int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Credit{UID: uid, Remain: remain, CheckedAt: time.Now().UTC()}
	if used != nil {
		n := *used
		c.Used = &n
	}
	index := -1
	for i := range s.credits {
		if s.credits[i].UID == uid {
			index = i
			break
		}
	}
	if index < 0 {
		s.credits = append(s.credits, c)
	} else {
		s.credits[index] = c
	}
	err := s.persist()
	s.lastError = err != nil
	return err
}

func (s *Store) Credits() []Credit {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]Credit{}, s.credits...)
	for i := range result {
		if result[i].Used != nil {
			n := *result[i].Used
			result[i].Used = &n
		}
	}
	return result
}

// Record persists daily aggregates without storing credentials or prompts.
func (s *Store) Record(key string, status int, input, output int64, known bool, cached *int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Now().UTC().Format("2006-01-02")
	index := -1
	for i, row := range s.rows {
		if row.Day == day && row.KeyID == key {
			index = i
			break
		}
	}
	if index < 0 {
		s.rows = append(s.rows, Row{Day: day, KeyID: key})
		index = len(s.rows) - 1
	}
	r := &s.rows[index]
	r.Requests++
	if status >= 400 {
		r.Failures++
	}
	if known {
		r.Input += input
		r.Output += output
	} else {
		r.Missing++
	}
	if cached != nil && *cached >= 0 && (!known || *cached <= input) {
		r.Cached += *cached
	} else {
		r.CacheMissing++
	}
	err := s.persist()
	s.lastError = err != nil
	return err
}

func (s *Store) Snapshot(from, to string) ([]Row, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := []Row{}
	for _, r := range s.rows {
		if (from == "" || r.Day >= from) && (to == "" || r.Day <= to) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Day == rows[j].Day {
			return rows[i].KeyID < rows[j].KeyID
		}
		return rows[i].Day < rows[j].Day
	})
	return rows, s.lastError
}

func (s *Store) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(diskState{Rows: s.rows, Credits: s.credits})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".usage-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
