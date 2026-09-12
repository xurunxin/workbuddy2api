package usage

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPersistenceAndConcurrentAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Record("key-a", 200, 3, 4, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := s.Record("key-b", 502, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, bad := s.Snapshot("", "")
	if bad || len(rows) != 2 {
		t.Fatalf("rows=%v bad=%v", rows, bad)
	}
	if rows[0].Requests != 20 || rows[0].Input != 60 || rows[0].Output != 80 || rows[1].Failures != 1 || rows[1].Missing != 1 {
		t.Fatal(rows)
	}
	rows[0].Requests = 999
	again, _ := s.Snapshot("", "")
	if again[0].Requests != 20 {
		t.Fatal("snapshot aliases state")
	}
	none, _ := s.Snapshot("9999-01-01", "")
	if len(none) != 0 {
		t.Fatal(none)
	}
}

func TestPersistenceFailureVisibleAndRecovered(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Store{path: filepath.Join(blocked, "usage.json")}
	if err := s.Record("key", 200, 2, 3, true); err == nil {
		t.Fatal("expected persistence error")
	}
	rows, bad := s.Snapshot("", "")
	if !bad || len(rows) != 1 {
		t.Fatal(rows, bad)
	}
	s.path = filepath.Join(dir, "usage.json")
	if err := s.Record("key", 200, 2, 3, true); err != nil {
		t.Fatal(err)
	}
	rows, bad = s.Snapshot("", "")
	if bad || rows[0].Requests != 2 {
		t.Fatal(rows, bad)
	}
}
