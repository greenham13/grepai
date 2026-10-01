package cli

import (
	"context"
	"encoding/gob"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/indexer"
	"github.com/yoanbernabeu/grepai/store"
	"github.com/yoanbernabeu/grepai/trace"
	"github.com/yoanbernabeu/grepai/watcher"
)

func readWatchSnapshot(t *testing.T, root string) map[string]struct {
	Hash   string
	Chunks int
} {
	t.Helper()
	f, err := os.Open(filepath.Join(config.GetConfigDir(root), "scan-snapshot.gob"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries map[string]struct {
		Hash   string
		Chunks int
	}
	if err := gob.NewDecoder(f).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestHandleFileEventPersistsScanSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "main.go")
	if err := os.WriteFile(source, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ignore, err := indexer.NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	scanner := indexer.NewScanner(root, ignore)
	st := store.NewGOBStore(config.GetIndexPath(root))
	symbols := trace.NewGOBSymbolStore(config.GetSymbolIndexPath(root))
	idx := indexer.NewIndexer(root, st, &noOpEmbedder{}, indexer.NewChunker(512, 50), scanner, time.Time{})
	cfg := config.DefaultConfig()
	extractor := trace.NewRegexExtractor()
	var lastConfigWrite time.Time
	event := func(kind watcher.EventType) {
		handleFileEvent(ctx, idx, scanner, extractor, symbols, nil, st, nil, root, cfg, &lastConfigWrite, nil, watcher.FileEvent{Type: kind, Path: "main.go"}, nil, nil)
	}
	event(watcher.EventCreate)
	first := readWatchSnapshot(t, root)["main.go"]
	if first.Hash == "" || first.Chunks == 0 {
		t.Fatalf("snapshot=%+v", first)
	}
	if err := os.WriteFile(source, []byte("package main\nfunc main() { println(42) }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	event(watcher.EventModify)
	// A periodic flush must save pending event updates even with no more events.
	if err := persistProjectPeriodically(ctx, newWatchMutationFence(), st, symbols, nil, root, idx); err != nil {
		t.Fatal(err)
	}
	second := readWatchSnapshot(t, root)["main.go"]
	if second.Hash == "" || first.Hash == second.Hash {
		t.Fatalf("snapshot was not updated: %+v", second)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	event(watcher.EventDelete)
	runtimes := map[string]*workspaceProjectRuntime{"project": {project: config.ProjectEntry{Name: "project", Path: root}, idx: idx, symbolStore: symbols}}
	if err := persistWorkspaceStores(ctx, st, runtimes); err != nil {
		t.Fatal(err)
	}
	if len(readWatchSnapshot(t, root)) != 0 {
		t.Fatal("shutdown retained deleted file in snapshot")
	}
}
