package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yoanbernabeu/grepai/trace"
)

func TestSymbolCacheTracksBaseAndLog(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "symbols.gob")
	writer := trace.NewGOBSymbolStore(path)
	save := func(file string) {
		t.Helper()
		if err := writer.SaveFile(ctx, file, []trace.Symbol{{Name: file, File: file}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := writer.Persist(ctx); err != nil {
			t.Fatal(err)
		}
	}
	save("base.go")
	var cache symbolCache
	first, err := cache.load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	hit, err := cache.load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if first != hit {
		t.Fatal("unchanged store missed cache")
	}
	save("log.go")
	second, err := cache.load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if second == first || !second.IsFileIndexed("log.go") {
		t.Fatal("log append missed")
	}
	// A new writer replaces the base and removes the old log.
	writer = trace.NewGOBSymbolStore(path)
	save("new.go")
	third, err := cache.load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if third == second || !third.IsFileIndexed("new.go") || third.IsFileIndexed("base.go") {
		t.Fatal("base replacement missed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	empty, err := cache.load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if empty.IsFileIndexed("new.go") {
		t.Fatal("deleted base served stale data")
	}
}

func TestSymbolCacheConcurrentAndFailedLoads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "symbols.gob")
	writer := trace.NewGOBSymbolStore(path)
	if err := writer.SaveFile(ctx, "a.go", []trace.Symbol{{Name: "A", File: "a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	var cache symbolCache
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := cache.load(ctx, path)
			if err != nil {
				t.Error(err)
				return
			}
			if !s.IsFileIndexed("a.go") {
				t.Error("missing file")
			}
		}()
	}
	wg.Wait()
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.load(ctx, path); err == nil {
		t.Fatal("bad store accepted")
	}
	if _, ok := cache.entries[path]; ok {
		t.Fatal("failed load cached")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := cache.load(cancelCtx, path); err != context.Canceled {
		t.Fatalf("canceled: %v", err)
	}
}
