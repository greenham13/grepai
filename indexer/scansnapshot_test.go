package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/store"
)

func snapshotFixture(t *testing.T) (*Indexer, *mockStore, *atomic.Int64) {
	t.Helper()
	root := t.TempDir()
	createGoFixtureFiles(t, root, 2)
	// Fixed, portable precision also makes same-second changes deterministic.
	stamp := time.Unix(1700000000, 100000000)
	for _, path := range []string{"file_0000.go", "file_0001.go"} {
		if err := os.Chtimes(filepath.Join(root, path), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	ignore, err := NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(root, ignore)
	reads := &atomic.Int64{}
	scanner.readFile = func(path string) ([]byte, error) { reads.Add(1); return os.ReadFile(path) }
	st := newMockStore()
	idx := NewIndexer(root, st, newMockEmbedder(), NewChunker(512, 50), scanner, time.Time{})
	if _, err := idx.IndexAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return idx, st, reads
}

func TestScanSnapshotUnchangedRestart(t *testing.T) {
	idx, st, reads := snapshotFixture(t)
	if _, err := os.Stat(idx.snapshot.path); err != nil {
		t.Fatal(err)
	}
	idx = NewIndexer(idx.root, st, newMockEmbedder(), idx.chunker, idx.scanner, time.Time{})
	reads.Store(0)
	st.getDocCalls = 0
	stats, err := idx.IndexAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.getDocCalls != 0 || reads.Load() != 0 || stats.FilesSkipped != 2 {
		t.Fatalf("lookups=%d reads=%d stats=%+v", st.getDocCalls, reads.Load(), stats)
	}
}

func TestScanSnapshotChangedFile(t *testing.T) {
	for _, change := range []string{"mtime", "size", "branch-switch-same-content"} {
		t.Run(change, func(t *testing.T) {
			idx, st, reads := snapshotFixture(t)
			path := "file_0000.go"
			abs := filepath.Join(idx.root, path)
			before, _ := idx.snapshot.get(path)
			data, err := os.ReadFile(abs)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "mtime":
				data[len(data)-2] = ' ' // Same size, different content.
			case "size":
				data = append(data, []byte("\n// modified\n")...)
			}
			if err := os.WriteFile(abs, data, 0644); err != nil {
				t.Fatal(err)
			}
			// Exercise sub-second mtime changes and changes older than lastIndexTime.
			stamp := time.Unix(0, before.ModTime).Add(time.Millisecond)
			if change == "size" {
				stamp = time.Unix(0, before.ModTime)
			}
			if err := os.Chtimes(abs, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			idx.lastIndexTime = stamp.Add(time.Hour)
			reads.Store(0)
			st.getDocCalls = 0
			stats, err := idx.IndexAll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if change == "branch-switch-same-content" {
				expected = 0
			}
			if stats.FilesIndexed != expected || reads.Load() != 1 || st.getDocCalls != 1 {
				t.Fatalf("lookups=%d reads=%d stats=%+v", st.getDocCalls, reads.Load(), stats)
			}
			after, ok := idx.snapshot.get(path)
			if !ok || after.ModTime != stamp.UnixNano() || after.Size != int64(len(data)) || after.Chunks == 0 || after.Hash != st.documents[path].Hash {
				t.Fatalf("snapshot entry=%+v", after)
			}
			if change != "branch-switch-same-content" && before.Hash == after.Hash {
				t.Fatal("modified content was not rehashed")
			}
		})
	}
}

func TestScanSnapshotRebuiltStore(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "empty", false: "partial"}[empty], func(t *testing.T) {
			idx, st, reads := snapshotFixture(t)
			delete(st.documents, "file_0000.go")
			if empty {
				st.documents = make(map[string]store.Document)
				st.chunks = make(map[string]store.Chunk)
			}
			reads.Store(0)
			st.getDocCalls = 0
			stats, err := idx.IndexAll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if empty {
				want = 2
			}
			if stats.FilesIndexed != want || st.getDocCalls != want || reads.Load() != int64(want) {
				t.Fatalf("lookups=%d reads=%d stats=%+v", st.getDocCalls, reads.Load(), stats)
			}
		})
	}
}

func TestScanSnapshotWithoutChunksDoesNotSkip(t *testing.T) {
	idx, st, reads := snapshotFixture(t)
	path := "file_0000.go"
	entry, _ := idx.snapshot.get(path)
	entry.Chunks = 0
	idx.snapshot.entries[path] = entry
	doc := st.documents[path]
	doc.ChunkIDs = nil
	st.documents[path] = doc
	reads.Store(0)
	st.getDocCalls = 0
	stats, err := idx.IndexAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesIndexed != 1 || st.getDocCalls != 1 || reads.Load() != 1 {
		t.Fatalf("lookups=%d reads=%d stats=%+v", st.getDocCalls, reads.Load(), stats)
	}
}

func TestScanSnapshotCorrupt(t *testing.T) {
	idx, st, reads := snapshotFixture(t)
	if err := os.WriteFile(idx.snapshot.path, []byte("not a gob"), 0600); err != nil {
		t.Fatal(err)
	}
	idx = NewIndexer(idx.root, st, newMockEmbedder(), idx.chunker, idx.scanner, time.Time{})
	if len(idx.snapshot.entries) != 0 {
		t.Fatal("corrupt snapshot retained entries")
	}
	reads.Store(0)
	if _, err := idx.IndexAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 {
		t.Fatalf("reads=%d", reads.Load())
	}
	if len(loadScanSnapshot(idx.root).entries) != 2 {
		t.Fatal("snapshot not repaired")
	}
}

func TestScanSnapshotDeletedFile(t *testing.T) {
	idx, st, _ := snapshotFixture(t)
	if err := os.Remove(filepath.Join(idx.root, "file_0000.go")); err != nil {
		t.Fatal(err)
	}
	stats, err := idx.IndexAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesRemoved != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	if _, ok := loadScanSnapshot(idx.root).get("file_0000.go"); ok {
		t.Fatal("deleted file retained in snapshot")
	}
	if _, ok := st.documents["file_0000.go"]; ok {
		t.Fatal("deleted file retained in store")
	}
}

func TestScanSnapshotCoalescesAndFlushes(t *testing.T) {
	idx, _, _ := snapshotFixture(t)
	idx.snapshot.lastSave = time.Now().Add(time.Hour)
	idx.snapshot.remove("file_0000.go")
	if err := idx.PersistScanSnapshot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadScanSnapshot(idx.root).get("file_0000.go"); !ok {
		t.Fatal("event write was not coalesced")
	}
	if err := idx.PersistScanSnapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadScanSnapshot(idx.root).get("file_0000.go"); ok {
		t.Fatal("forced flush did not save pending change")
	}
	idx.snapshot.remove("file_0001.go")
	idx.snapshot.lastSave = time.Now().Add(-scanSnapshotInterval)
	if err := idx.PersistScanSnapshot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(loadScanSnapshot(idx.root).entries) != 0 {
		t.Fatal("due event write did not flush")
	}
}

func TestScanSnapshotWriteFailureRetainsDirtyState(t *testing.T) {
	s := loadScanSnapshot(t.TempDir())
	s.record(FileInfo{Path: "x.go", ModTimeNano: 1}, 1)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	original := s.path
	s.path = filepath.Join(blocker, "snapshot")
	if err := s.save(true); err == nil {
		t.Fatal("expected write error")
	}
	if !s.dirty {
		t.Fatal("failed write cleared dirty state")
	}
	s.path = original
	if err := s.save(true); err != nil {
		t.Fatal(err)
	}
}

func TestScanSnapshotBatchedIndex(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "save-failure"}[fail], func(t *testing.T) {
			idx, st, reads := snapshotFixture(t)
			idx.embedder = newMockBatchEmbedder()
			st.documents = make(map[string]store.Document)
			st.chunks = make(map[string]store.Chunk)
			if fail {
				idx.store = &snapshotFailingStore{st}
			}
			_, err := idx.IndexAll(context.Background())
			if fail {
				if err == nil {
					t.Fatal("expected save failure")
				}
				if len(idx.snapshot.entries) != 0 {
					t.Fatal("failed buffered save populated snapshot")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(loadScanSnapshot(idx.root).entries) != 2 {
				t.Fatal("successful batch did not populate snapshot")
			}
			st.getDocCalls = 0
			reads.Store(0)
			if _, err := idx.IndexAll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if st.getDocCalls != 0 || reads.Load() != 0 {
				t.Fatalf("lookups=%d reads=%d", st.getDocCalls, reads.Load())
			}
		})
	}
}

type snapshotPersistFailingStore struct{ *mockStore }

func (s *snapshotPersistFailingStore) Persist(context.Context) error {
	return errors.New("persist failed")
}

func TestScanSnapshotWaitsForVectorPersistence(t *testing.T) {
	idx, st, _ := snapshotFixture(t)
	idx.snapshot.remove("file_0000.go")
	idx.store = &snapshotPersistFailingStore{st}
	if err := idx.PersistScanSnapshot(context.Background(), true); err == nil {
		t.Fatal("expected vector persist failure")
	}
	if _, ok := loadScanSnapshot(idx.root).get("file_0000.go"); !ok {
		t.Fatal("snapshot published before vector data was durable")
	}
	if !idx.snapshot.dirty {
		t.Fatal("failure cleared pending changes")
	}
	idx.store = st
	if err := idx.PersistScanSnapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadScanSnapshot(idx.root).get("file_0000.go"); ok {
		t.Fatal("retry did not publish snapshot")
	}
}

type snapshotFailingStore struct{ *mockStore }

func (s *snapshotFailingStore) SaveDocument(context.Context, store.Document) error {
	return errors.New("save failed")
}

func TestScanSnapshotFailedIndexInvalidatesEntry(t *testing.T) {
	idx, st, _ := snapshotFixture(t)
	file, err := idx.scanner.ScanFile("file_0000.go")
	if err != nil {
		t.Fatal(err)
	}
	idx.store = &snapshotFailingStore{st}
	if _, err := idx.IndexFile(context.Background(), *file); err == nil {
		t.Fatal("expected save error")
	}
	if _, ok := idx.snapshot.get(file.Path); ok {
		t.Fatal("failed reindex retained snapshot entry")
	}
}
