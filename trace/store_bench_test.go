package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The synthetic store: 4,000 files x 50 symbols = 200k symbols, with 100
// references (and call edges) per file drawn from a shared name pool.
const (
	benchFiles       = 4000
	benchSymsPerFile = 50
	benchRefsPerFile = 100
)

var (
	benchOnce  sync.Once
	benchDir   string
	benchStore *GOBSymbolStore
)

func benchLargeStore(b *testing.B) *GOBSymbolStore {
	b.Helper()
	benchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "grepai-symbol-bench-")
		if err != nil {
			b.Fatal(err)
		}
		benchDir = dir
		ctx := context.Background()
		benchStore = NewGOBSymbolStore(filepath.Join(dir, "symbols.gob"))
		for f := 0; f < benchFiles; f++ {
			file := fmt.Sprintf("pkg%d/file%d.go", f%50, f)
			symbols, refs := syntheticFile(file, f, benchSymsPerFile, benchRefsPerFile, 0)
			if err := benchStore.SaveFileWithSignature(ctx, file, "h", "v1", symbols, refs); err != nil {
				b.Fatal(err)
			}
		}
		if err := benchStore.Persist(ctx); err != nil {
			b.Fatal(err)
		}
	})
	return benchStore
}

func BenchmarkSymbolStore200kUpdateOneFile(b *testing.B) {
	store := benchLargeStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := i % benchFiles
		file := fmt.Sprintf("pkg%d/file%d.go", f%50, f)
		symbols, refs := syntheticFile(file, f, benchSymsPerFile, benchRefsPerFile, 1+i%2)
		if err := store.SaveFileWithSignature(ctx, file, fmt.Sprint(i), "v1", symbols, refs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSymbolStore200kResaveUnchangedFile(b *testing.B) {
	store := benchLargeStore(b)
	ctx := context.Background()
	file := "pkg11/file11.go"
	symbols, refs := syntheticFile(file, 11, benchSymsPerFile, benchRefsPerFile, 0)
	if err := store.SaveFileWithSignature(ctx, file, "same", "v1", symbols, refs); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.SaveFileWithSignature(ctx, file, "same", "v1", symbols, refs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSymbolStore200kDeleteFile(b *testing.B) {
	store := benchLargeStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := i % benchFiles
		file := fmt.Sprintf("pkg%d/file%d.go", f%50, f)
		b.StopTimer()
		symbols, refs := syntheticFile(file, f, benchSymsPerFile, benchRefsPerFile, 0)
		if err := store.SaveFileWithSignature(ctx, file, "h", "v1", symbols, refs); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := store.DeleteFile(ctx, file); err != nil {
			b.Fatal(err)
		}
	}
}

// One changed file followed by the periodic persist.
func BenchmarkSymbolStore200kPersistOneChange(b *testing.B) {
	store := benchLargeStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := i % benchFiles
		file := fmt.Sprintf("pkg%d/file%d.go", f%50, f)
		symbols, refs := syntheticFile(file, f, benchSymsPerFile, benchRefsPerFile, 3+i%2)
		if err := store.SaveFileWithSignature(ctx, file, fmt.Sprint(i), "v1", symbols, refs); err != nil {
			b.Fatal(err)
		}
		if err := store.Persist(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if benchDir != "" {
		_ = os.RemoveAll(benchDir)
	}
	os.Exit(code)
}
