package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStateSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := &daemonState{Actions: map[string]*actionState{}}
	when := time.Date(2026, 7, 24, 6, 0, 0, 0, time.Local)
	st.setLastRun("sync", when)
	st.setStatus("sync", "completed")
	st.beat(when)
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	got := loadState(path)
	if !got.lastRun("sync").Equal(when) || got.lastStatus("sync") != "completed" || !got.heartbeatAt().Equal(when) {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// Mirrors the daemon's real call pattern: the tick goroutine stamps and saves
// while fire goroutines set statuses. Run under -race in CI.
func TestStateConcurrentAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := &daemonState{Actions: map[string]*actionState{}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := string(rune('a' + n%4))
			for j := 0; j < 200; j++ {
				st.setLastRun(name, time.Now())
				st.setStatus(name, "completed")
				st.lastRun(name)
				st.beat(time.Now())
				if err := st.save(path); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if got := loadState(path); got.Actions == nil {
		t.Fatal("state unreadable after concurrent saves")
	}
}

func TestLoadStatePreservesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	st := loadState(path)
	if len(st.Actions) != 0 {
		t.Errorf("corrupt state should load empty")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("corrupt file should be preserved: %v", err)
	}
}

func TestStatePrune(t *testing.T) {
	st := &daemonState{Actions: map[string]*actionState{}}
	st.setLastRun("keep", time.Now())
	st.setLastRun("gone", time.Now())
	st.prune(map[string]bool{"keep": true})
	if !st.lastRun("gone").IsZero() || st.lastRun("keep").IsZero() {
		t.Errorf("prune kept the wrong entries: %+v", st.Actions)
	}
}
