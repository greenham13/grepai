package trace

import (
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fileSnapshot struct {
	info os.FileInfo
}

func snapshotFile(t *testing.T, path string) fileSnapshot {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fileSnapshot{info: info}
}

// sameFile reports whether path is still the very file captured earlier:
// same inode, size and modification time.
func (f fileSnapshot) sameFile(t *testing.T, path string) bool {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return os.SameFile(f.info, info) && f.info.Size() == info.Size() && f.info.ModTime().Equal(info.ModTime())
}

func testFileEntries(file string, variant int) ([]Symbol, []Reference) {
	symbols := []Symbol{
		{Name: "Run", Kind: KindFunction, File: file, Line: 1 + variant, Language: "go"},
		{Name: "helper", Kind: KindFunction, File: file, Line: 10 + variant, Language: "go"},
	}
	refs := []Reference{
		{SymbolName: "helper", Kind: RefKindCall, File: file, Line: 2 + variant, CallerName: "Run", CallerFile: file},
		{SymbolName: "Println", Kind: RefKindCall, File: file, Line: 3 + variant, CallerName: "Run", CallerFile: file},
		{SymbolName: "Println", Kind: RefKindCall, File: file, Line: 11 + variant, CallerName: "helper", CallerFile: file},
	}
	return symbols, refs
}

func TestGOBSymbolStore_UnchangedResaveDoesNotDirtyOrPersist(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	store := NewGOBSymbolStore(indexPath)
	symbols, refs := testFileEntries("a.go", 0)
	if err := store.SaveFileWithSignature(ctx, "a.go", "h1", "v1", symbols, refs); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	base := snapshotFile(t, indexPath)

	// The extractor returns fresh slices with equal content on every run.
	again, againRefs := testFileEntries("a.go", 0)
	if err := store.SaveFileWithSignature(ctx, "a.go", "h1", "v1", again, againRefs); err != nil {
		t.Fatal(err)
	}
	if store.mutationGeneration != 0 {
		t.Fatalf("identical re-save bumped mutation generation to %d", store.mutationGeneration)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if !base.sameFile(t, indexPath) {
		t.Fatal("identical re-save rewrote symbols.gob")
	}
	if _, err := os.Stat(indexPath + ".log"); !os.IsNotExist(err) {
		t.Fatalf("identical re-save wrote a log: %v", err)
	}
}

func TestGOBSymbolStore_VersionBackfillAppendsLogOnly(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	// A store written before extractor versions were recorded.
	legacy := NewGOBSymbolStore(indexPath)
	for i := 0; i < 3; i++ {
		file := fmt.Sprintf("f%d.go", i)
		symbols, refs := testFileEntries(file, 0)
		if err := legacy.SaveFileWithContentHash(ctx, file, "h", symbols, refs); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	base := snapshotFile(t, indexPath)

	store := NewGOBSymbolStore(indexPath)
	if err := store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		file := fmt.Sprintf("f%d.go", i)
		symbols, refs := testFileEntries(file, 0)
		if err := store.SaveFileWithSignature(ctx, file, "h", "v2", symbols, refs); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if !base.sameFile(t, indexPath) {
		t.Fatal("extractor-version backfill rewrote the whole symbols.gob")
	}
	if _, err := os.Stat(indexPath + ".log"); err != nil {
		t.Fatalf("backfill did not reach the log: %v", err)
	}

	reloaded := NewGOBSymbolStore(indexPath)
	if err := reloaded.Load(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		file := fmt.Sprintf("f%d.go", i)
		if v, ok := reloaded.GetFileExtractorVersion(file); !ok || v != "v2" {
			t.Fatalf("%s version=%q,%v after reload", file, v, ok)
		}
	}
	if syms, _ := reloaded.LookupSymbol(ctx, "Run"); len(syms) != 3 {
		t.Fatalf("metadata-only records lost entries: Run=%d", len(syms))
	}
}

func TestGOBSymbolStore_IncrementalChangesSurviveReload(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	store := NewGOBSymbolStore(indexPath)
	for _, file := range []string{"a.go", "b.go", "c.go"} {
		symbols, refs := testFileEntries(file, 0)
		if err := store.SaveFileWithSignature(ctx, file, "h-"+file, "v1", symbols, refs); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	base := snapshotFile(t, indexPath)

	symbols, refs := testFileEntries("a.go", 100)
	symbols = append(symbols, Symbol{Name: "Added", Kind: KindFunction, File: "a.go", Line: 500})
	if err := store.SaveFileWithSignature(ctx, "a.go", "h-a2", "v1", symbols, refs); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteFile(ctx, "b.go"); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if !base.sameFile(t, indexPath) {
		t.Fatal("small change rewrote the base snapshot")
	}

	reloaded := NewGOBSymbolStore(indexPath)
	if err := reloaded.Load(ctx); err != nil {
		t.Fatal(err)
	}
	want, _ := store.GetCallEdges(ctx)
	got, _ := reloaded.GetCallEdges(ctx)
	if fmt.Sprint(want) != fmt.Sprint(got) {
		t.Fatalf("edges after reload:\n got %v\nwant %v", got, want)
	}
	if reloaded.IsFileIndexed("b.go") {
		t.Fatal("deleted file came back after reload")
	}
	if h, _ := reloaded.GetFileContentHash("a.go"); h != "h-a2" {
		t.Fatalf("hash=%q", h)
	}
	added, _ := reloaded.LookupSymbol(ctx, "Added")
	if len(added) != 1 {
		t.Fatalf("Added=%v", added)
	}
	callers, _ := reloaded.LookupCallers(ctx, "helper")
	if len(callers) != 2 || callers[0].File != "a.go" || callers[0].Line != 102 || callers[1].File != "c.go" {
		t.Fatalf("helper callers=%+v", callers)
	}
	stats, _ := reloaded.GetStats(ctx)
	if stats.TotalFiles != 2 || stats.TotalSymbols != 5 || stats.TotalReferences != 6 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestGOBSymbolStore_TornLogTailIsDroppedAndOverwritten(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	logPath := indexPath + ".log"
	store := NewGOBSymbolStore(indexPath)
	symbols, refs := testFileEntries("a.go", 0)
	if err := store.SaveFile(ctx, "a.go", symbols, refs); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFile(ctx, "b.go", []Symbol{{Name: "B", File: "b.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	good := snapshotFile(t, logPath).info.Size()
	if err := store.SaveFile(ctx, "c.go", []Symbol{{Name: "C", File: "c.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash in the middle of writing the last frame.
	if err := os.Truncate(logPath, snapshotFile(t, logPath).info.Size()-3); err != nil {
		t.Fatal(err)
	}

	writer := NewGOBSymbolStore(indexPath)
	if err := writer.Load(ctx); err != nil {
		t.Fatalf("torn tail must not fail Load: %v", err)
	}
	if !writer.IsFileIndexed("b.go") || writer.IsFileIndexed("c.go") {
		t.Fatalf("replay should keep b.go and drop torn c.go: b=%v c=%v", writer.IsFileIndexed("b.go"), writer.IsFileIndexed("c.go"))
	}
	if writer.logSize != good {
		t.Fatalf("valid log prefix=%d want %d", writer.logSize, good)
	}
	if err := writer.SaveFile(ctx, "d.go", []Symbol{{Name: "D", File: "d.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	check := NewGOBSymbolStore(indexPath)
	if err := check.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !check.IsFileIndexed("b.go") || !check.IsFileIndexed("d.go") {
		t.Fatal("append after a torn tail was not replayed")
	}
}

// oldGobSymbolData is the snapshot shape of releases before the change log.
type oldGobSymbolData struct {
	Index                 SymbolIndex
	FileIndex             map[string]bool
	FileContentHashes     map[string]string
	FileExtractorVersions map[string]string
}

func TestGOBSymbolStore_BaseStaysReadableAndForeignBaseOrphansLog(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	store := NewGOBSymbolStore(indexPath)
	symbols, refs := testFileEntries("a.go", 0)
	if err := store.SaveFileWithSignature(ctx, "a.go", "h", "v1", symbols, refs); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFile(ctx, "b.go", []Symbol{{Name: "B", File: "b.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}

	// A release without log support decodes the base...
	f, err := os.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var old oldGobSymbolData
	err = gob.NewDecoder(f).Decode(&old)
	f.Close()
	if err != nil {
		t.Fatalf("old release cannot decode base: %v", err)
	}
	if len(old.Index.Symbols["Run"]) != 1 || old.FileExtractorVersions["a.go"] != "v1" {
		t.Fatalf("old decode lost data: %+v", old.Index.Symbols)
	}
	// ...and rewrites it without a LogID. Our log must not be replayed on it.
	old.Index.Symbols["Old"] = []Symbol{{Name: "Old", File: "old.go"}}
	old.FileIndex["old.go"] = true
	tmp := indexPath + ".old"
	out, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(out).Encode(old); err != nil {
		t.Fatal(err)
	}
	out.Close()
	if err := os.Rename(tmp, indexPath); err != nil {
		t.Fatal(err)
	}

	reloaded := NewGOBSymbolStore(indexPath)
	if err := reloaded.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if reloaded.IsFileIndexed("b.go") || !reloaded.IsFileIndexed("old.go") {
		t.Fatal("log written for another base was replayed")
	}
	// The next persist cannot extend a base without a LogID: it rewrites.
	if err := reloaded.SaveFile(ctx, "c.go", []Symbol{{Name: "C", File: "c.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexPath + ".log"); !os.IsNotExist(err) {
		t.Fatalf("stale log survived compaction: %v", err)
	}
	check := NewGOBSymbolStore(indexPath)
	if err := check.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !check.IsFileIndexed("c.go") || !check.IsFileIndexed("old.go") || check.IsFileIndexed("b.go") {
		t.Fatal("compaction state wrong")
	}
}

func TestGOBSymbolStore_LogCompactsWhenLarge(t *testing.T) {
	saved := symbolLogCompactMinBytes
	symbolLogCompactMinBytes = 2048
	defer func() { symbolLogCompactMinBytes = saved }()

	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	store := NewGOBSymbolStore(indexPath)
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	base := snapshotFile(t, indexPath)
	compacted := false
	for i := 0; i < 50 && !compacted; i++ {
		symbols, refs := testFileEntries(fmt.Sprintf("f%d.go", i), i)
		if err := store.SaveFile(ctx, fmt.Sprintf("f%d.go", i), symbols, refs); err != nil {
			t.Fatal(err)
		}
		if err := store.Persist(ctx); err != nil {
			t.Fatal(err)
		}
		compacted = !base.sameFile(t, indexPath)
	}
	if !compacted {
		t.Fatal("log never folded into the base")
	}
	if _, err := os.Stat(indexPath + ".log"); !os.IsNotExist(err) {
		t.Fatalf("log left after compaction: %v", err)
	}
	check := NewGOBSymbolStore(indexPath)
	if err := check.Load(ctx); err != nil {
		t.Fatal(err)
	}
	want, _ := store.GetStats(ctx)
	got, _ := check.GetStats(ctx)
	if got.TotalFiles != want.TotalFiles || got.TotalSymbols != want.TotalSymbols || got.TotalReferences != want.TotalReferences {
		t.Fatalf("after compaction got %+v want %+v", got, want)
	}
}

func TestGOBSymbolStore_RepeatedEditsBoundSmallBaseLog(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	store := NewGOBSymbolStore(indexPath)
	name := strings.Repeat("x", 32<<10)
	save := func(line int) {
		t.Helper()
		if err := store.SaveFile(ctx, "a.go", []Symbol{{Name: name, File: "a.go", Line: line}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	save(1)
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if size := snapshotFile(t, indexPath).info.Size(); size >= 1<<20 {
		t.Fatalf("base is not small: %d bytes", size)
	}

	merges := 0
	for line := 2; line <= 100; line++ {
		base := snapshotFile(t, indexPath)
		limit := max(int64(1<<20), base.info.Size()/4)
		save(line)
		frame, err := store.encodeDirtyBatchUnlocked(store.now())
		if err != nil {
			t.Fatal(err)
		}
		nextSize := store.logSize
		if nextSize == 0 {
			nextSize = int64(len(symbolLogHeader(store.logID)))
		}
		nextSize += int64(len(frame))
		if err := store.Persist(ctx); err != nil {
			t.Fatal(err)
		}
		merged := !base.sameFile(t, indexPath)
		if merged != (nextSize > limit) {
			t.Fatalf("edit %d: merged=%v, projected log=%d, limit=%d", line, merged, nextSize, limit)
		}
		if merged {
			merges++
		}
		info, err := os.Stat(indexPath + ".log")
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err == nil && info.Size() > limit+int64(len(frame)) {
			t.Fatalf("edit %d: log exceeds limit plus one record: %d", line, info.Size())
		}
	}
	if merges < 2 {
		t.Fatalf("repeated edits merged only %d times", merges)
	}
	reloaded := NewGOBSymbolStore(indexPath)
	if err := reloaded.Load(ctx); err != nil {
		t.Fatal(err)
	}
	symbols, err := reloaded.LookupSymbol(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 1 || symbols[0].Line != 100 {
		t.Fatalf("latest edit lost after merging: %+v", symbols)
	}
}

func buildLargeStore(t testing.TB, files, symsPerFile, refsPerFile int) *GOBSymbolStore {
	t.Helper()
	ctx := context.Background()
	store := NewGOBSymbolStore(filepath.Join(t.TempDir(), "symbols.gob"))
	for f := 0; f < files; f++ {
		file := fmt.Sprintf("pkg%d/file%d.go", f%50, f)
		symbols, refs := syntheticFile(file, f, symsPerFile, refsPerFile, 0)
		if err := store.SaveFileWithSignature(ctx, file, "h", "v1", symbols, refs); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// syntheticFile builds one file's entries. Reference targets come from a
// shared pool, so popular names collect references from many files.
func syntheticFile(file string, f, symsPerFile, refsPerFile, shift int) ([]Symbol, []Reference) {
	symbols := make([]Symbol, 0, symsPerFile)
	for i := 0; i < symsPerFile; i++ {
		symbols = append(symbols, Symbol{Name: fmt.Sprintf("Sym%d_%d", f, i), Kind: KindFunction, File: file, Line: i*10 + 1 + shift, Language: "go"})
	}
	refs := make([]Reference, 0, refsPerFile)
	for i := 0; i < refsPerFile; i++ {
		refs = append(refs, Reference{
			SymbolName: fmt.Sprintf("Common%d", (f*7+i)%500),
			Kind:       RefKindCall,
			File:       file,
			Line:       i*5 + 2 + shift,
			CallerName: fmt.Sprintf("Sym%d_%d", f, i%symsPerFile),
			CallerFile: file,
		})
	}
	return symbols, refs
}

func TestGOBSymbolStore_FileUpdateTouchesOnlyItsEntries(t *testing.T) {
	ctx := context.Background()
	const files, syms, refs = 2000, 20, 40
	store := buildLargeStore(t, files, syms, refs)
	file := "pkg7/file7.go"
	symbols, references := syntheticFile(file, 7, syms, refs, 1)

	before := store.removalVisits
	if err := store.SaveFileWithSignature(ctx, file, "h2", "v1", symbols, references); err != nil {
		t.Fatal(err)
	}
	// The old symbols, references and call edges of this one file.
	if visited, want := store.removalVisits-before, uint64(syms+refs+refs); visited != want {
		t.Fatalf("update visited %d entries, want %d (store holds %d files)", visited, want, files)
	}
	before = store.removalVisits
	if err := store.DeleteFile(ctx, file); err != nil {
		t.Fatal(err)
	}
	if visited, want := store.removalVisits-before, uint64(syms+refs+refs); visited != want {
		t.Fatalf("delete visited %d entries, want %d", visited, want)
	}
	if got, _ := store.GetSymbolsForFile(ctx, file); len(got) != 0 {
		t.Fatalf("deleted file still has %d symbols", len(got))
	}
	callers, _ := store.LookupCallers(ctx, "Common0")
	for _, ref := range callers {
		if ref.File == file {
			t.Fatal("deleted file still referenced")
		}
	}
}

func TestGOBSymbolStore_RepeatedSavesDoNotGrowCallGraph(t *testing.T) {
	ctx := context.Background()
	store := buildLargeStore(t, 50, 5, 10)
	baseline, _ := store.GetCallEdges(ctx)
	for round := 0; round < 6; round++ {
		symbols, refs := syntheticFile("pkg3/file3.go", 3, 5, 10, round%2)
		if err := store.SaveFileWithSignature(ctx, "pkg3/file3.go", fmt.Sprint(round), "v1", symbols, refs); err != nil {
			t.Fatal(err)
		}
	}
	edges, _ := store.GetCallEdges(ctx)
	if len(edges) != len(baseline) {
		t.Fatalf("call graph grew from %d to %d edges", len(baseline), len(edges))
	}
	stats, _ := store.GetStats(ctx)
	if stats.TotalReferences != 50*10 || stats.TotalSymbols != 50*5 {
		t.Fatalf("stats drifted: %+v", stats)
	}
}

func TestGOBSymbolStore_UpdatedAtSurvivesLogReplay(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "symbols.gob")
	store := NewGOBSymbolStore(indexPath)
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFile(ctx, "a.go", []Symbol{{Name: "A", File: "a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	want, _ := store.GetStats(ctx)
	check := NewGOBSymbolStore(indexPath)
	if err := check.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := check.GetStats(ctx)
	if !got.LastUpdated.Equal(want.LastUpdated) {
		t.Fatalf("LastUpdated=%v want %v", got.LastUpdated, want.LastUpdated)
	}
}
