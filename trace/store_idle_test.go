package trace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSymbolIdleHourWritesNothing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "symbols.gob")
	s := NewGOBSymbolStore(path)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	saveReviewFile(t, s, "base.go")
	saveReviewFile(t, s, "changed.go")
	base := snapshotFile(t, path)
	log := snapshotFile(t, path+".log")
	for range 120 {
		now = now.Add(30 * time.Second)
		if err := s.Persist(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !base.sameFile(t, path) || !log.sameFile(t, path+".log") {
		t.Fatal("idle hour rewrote persisted symbols")
	}
	// Even a read-only store must be usable after the age threshold.
	if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0700) })
	if err := s.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	reader := NewGOBSymbolStore(path)
	if err := reader.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !reader.IsFileIndexed("changed.go") {
		t.Fatal("reader missed log update")
	}
}
