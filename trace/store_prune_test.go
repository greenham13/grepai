package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func pruneTestStore(t *testing.T, files int) (*GOBSymbolStore, string) {
	t.Helper()
	saved := symbolLogCompactMinBytes
	symbolLogCompactMinBytes = 4096
	t.Cleanup(func() { symbolLogCompactMinBytes = saved })

	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	store := NewGOBSymbolStore(indexPath)
	for f := 0; f < files; f++ {
		file := fmt.Sprintf("pkg/file%d.go", f)
		symbols, refs := syntheticFile(file, f, 10, 20, 0)
		if err := store.SaveFileWithSignature(ctx, file, "h", "v1", symbols, refs); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if size := snapshotFile(t, indexPath).info.Size(); size < symbolLogCompactMinBytes {
		t.Fatalf("base %d bytes is under the compaction floor", size)
	}
	return store, indexPath
}

func deleteFiles(t *testing.T, store *GOBSymbolStore, from, to int) {
	t.Helper()
	for f := from; f < to; f++ {
		if err := store.DeleteFile(context.Background(), fmt.Sprintf("pkg/file%d.go", f)); err != nil {
			t.Fatal(err)
		}
	}
}

func assertReloadHolds(t *testing.T, indexPath string, files int) {
	t.Helper()
	check := NewGOBSymbolStore(indexPath)
	if err := check.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(check.IndexedFiles()); got != files {
		t.Fatalf("reloaded store holds %d files, want %d", got, files)
	}
	if got, want := check.totalSymbols, files*10; got != want {
		t.Fatalf("reloaded store holds %d symbols, want %d", got, want)
	}
}

// F-510: deletes append tiny frames, so the log-size rule never folded a
// large prune into the base and every start decoded the old base in full.
func TestGOBSymbolStore_LargePruneRewritesSmallerBase(t *testing.T) {
	ctx := context.Background()
	store, indexPath := pruneTestStore(t, 200)
	before := snapshotFile(t, indexPath)

	// A small prune stays an append.
	deleteFiles(t, store, 0, 20)
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if !before.sameFile(t, indexPath) {
		t.Fatal("removing 10% of entries rewrote the base")
	}

	deleteFiles(t, store, 20, 120)
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	after := snapshotFile(t, indexPath)
	if before.sameFile(t, indexPath) {
		t.Fatal("removing 60% of entries left the base untouched")
	}
	if after.info.Size() >= before.info.Size()/2 {
		t.Fatalf("base shrank only from %d to %d bytes", before.info.Size(), after.info.Size())
	}
	if _, err := os.Stat(indexPath + ".log"); !os.IsNotExist(err) {
		t.Fatalf("log left after compaction: %v", err)
	}
	assertReloadHolds(t, indexPath, 80)
}

// A base already pruned through the log (by a release without the rule)
// is rewritten by the writer at startup, and never by a reader.
func TestGOBSymbolStore_CompactIfSparseShrinksPrunedBaseForWriterOnly(t *testing.T) {
	ctx := context.Background()
	store, indexPath := pruneTestStore(t, 200)
	deleteFiles(t, store, 0, 150)
	// The pre-fix persist: append the deletes, keep the base.
	frame, err := store.encodeDirtyBatchUnlocked(store.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.appendLogUnlocked(frame); err != nil {
		t.Fatal(err)
	}
	store.finishPersistUnlocked()
	before := snapshotFile(t, indexPath)

	reader := NewGOBSymbolStore(indexPath)
	if err := reader.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !before.sameFile(t, indexPath) {
		t.Fatal("a read-only Load/Close rewrote the base")
	}

	writer := NewGOBSymbolStore(indexPath)
	if err := writer.Load(ctx); err != nil {
		t.Fatal(err)
	}
	compacted, err := writer.CompactIfSparse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !compacted || before.sameFile(t, indexPath) {
		t.Fatalf("pruned base not rewritten (compacted=%v)", compacted)
	}
	if size := snapshotFile(t, indexPath).info.Size(); size >= before.info.Size()/2 {
		t.Fatalf("base shrank only from %d to %d bytes", before.info.Size(), size)
	}
	assertReloadHolds(t, indexPath, 50)

	again, err := writer.CompactIfSparse(ctx)
	if err != nil || again {
		t.Fatalf("second CompactIfSparse rewrote a dense base: compacted=%v err=%v", again, err)
	}
}
