package indexer

import "testing"

func TestScanSnapshotMutationHookOnlyOnChanges(t *testing.T) {
	s := &scanSnapshot{entries: make(map[string]scanSnapshotEntry)}
	idx := &Indexer{snapshot: s}
	calls := 0
	idx.SetMutationHook(func() { calls++ })
	file := FileInfo{Path: "a.go", Hash: "hash", Size: 42, ModTimeNano: 10}
	s.record(file, 1)
	s.record(file, 1)
	s.remove("missing.go")
	if calls != 1 {
		t.Fatalf("unchanged snapshot notified: %d", calls)
	}
	s.remove("a.go")
	if calls != 2 {
		t.Fatalf("delete notifications: %d", calls)
	}
	s.record(file, 1)
	s.reconcile(map[string]bool{})
	if calls != 4 {
		t.Fatalf("reconcile notifications: %d", calls)
	}
}
