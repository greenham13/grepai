package trace

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadedRepeatedTextKeepsTraceData(t *testing.T) {
	ctx := context.Background()
	s := NewGOBSymbolStore(filepath.Join(t.TempDir(), "symbols.gob"))
	text := strings.Repeat("shared text ", 100)
	sym := Symbol{Name: "Shared", Kind: KindFunction, File: "source.go", Line: 7, EndLine: 12, Signature: text, Receiver: text, Package: text, Exported: true, Language: "go", Docstring: text, FeaturePath: text}
	ref := Reference{SymbolName: "Shared", Kind: RefKindCall, File: "source.go", Line: 8, Column: 3, Context: text, CallerName: "Shared", CallerFile: "source.go", CallerLine: 7}
	if err := s.SaveFileWithSignature(ctx, "source.go", "hash", "extractor", []Symbol{sym}, []Reference{ref}); err != nil {
		t.Fatal(err)
	}
	if err := s.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	loaded := NewGOBSymbolStore(s.indexPath)
	if err := loaded.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := loaded.GetSymbolsForFile(ctx, "source.go")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Symbol{sym}) {
		t.Fatalf("changed symbols: %#v", got)
	}
	if !reflect.DeepEqual(loaded.refs, s.refs) || !reflect.DeepEqual(loaded.edges, s.edges) {
		t.Fatal("changed references or edges")
	}
	if hash, ok := loaded.GetFileContentHash("source.go"); !ok || hash != "hash" {
		t.Fatal("lost hash")
	}
	if version, ok := loaded.GetFileExtractorVersion("source.go"); !ok || version != "extractor" {
		t.Fatal("lost version")
	}
	if err := loaded.DeleteFile(ctx, "source.go"); err != nil {
		t.Fatal(err)
	}
	if err := loaded.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if s.IsFileIndexed("source.go") {
		t.Fatal("deleted shared text kept file")
	}
}
