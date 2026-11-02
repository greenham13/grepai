package indexer

import (
	"context"
	"errors"
	"github.com/yoanbernabeu/grepai/store"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testPolicy struct {
	exclude func(string) (bool, error)
	scope   string
}

func (p *testPolicy) Exclude(path string) (bool, error) { return p.exclude(path) }
func (p *testPolicy) Scope() string                     { return p.scope }
func policyFixture(t *testing.T, p PathPolicy) {
	t.Helper()
	SetPathPolicyFactory(func(string) (PathPolicy, error) { return p, nil })
	t.Cleanup(func() { SetPathPolicyFactory(nil) })
}
func putPolicyFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestPathPolicyWinsOverNegationsAndAppliesAfterConstruction(t *testing.T) {
	root := t.TempDir()
	policyFixture(t, &testPolicy{exclude: func(path string) (bool, error) { return path == "late" || strings.HasPrefix(path, "late/"), nil }})
	putPolicyFile(t, root, ".grepaiignore", "!*\n")
	putPolicyFile(t, root, "kept/.gitignore", "private.go\n")
	putPolicyFile(t, root, "kept/private.go", "package private")
	putPolicyFile(t, root, "kept/main.go", "package kept")
	putPolicyFile(t, root, "root.go", "package root")
	m, err := NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	putPolicyFile(t, root, "late/.grepaiignore", "!*\n")
	putPolicyFile(t, root, "late/file.go", "package leak")
	s := NewScanner(root, m)
	files, _, err := s.ScanMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("wanted root.go and kept/main.go: %+v", files)
	}
	s.readFile = func(string) ([]byte, error) { t.Fatal("read excluded source"); return nil, nil }
	if file, err := s.ScanFile("late/file.go"); err != nil || file != nil {
		t.Fatalf("%v %v", file, err)
	}
	if skip, err := m.ShouldSkipDirChecked("late"); err != nil || !skip {
		t.Fatalf("%v %v", skip, err)
	}
}

func TestPathPolicyErrorsPreserveDocuments(t *testing.T) {
	for _, stage := range []string{"walk", "worker", "cleanup", "reconcile-cleanup"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			putPolicyFile(t, root, "keep.go", "package keep")
			var fail atomic.Bool
			policyFixture(t, &testPolicy{exclude: func(path string) (bool, error) {
				if fail.Load() && (stage != "cleanup" && stage != "reconcile-cleanup" || path == "gone.go") {
					return false, os.ErrPermission
				}
				return false, nil
			}})
			m, err := NewIgnoreMatcher(root, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			s := NewScanner(root, m)
			st := newMockStore()
			ctx := context.Background()
			if err := st.SaveDocument(ctx, store.Document{Path: "gone.go", Hash: "old", ChunkIDs: []string{"old"}}); err != nil {
				t.Fatal(err)
			}
			idx := NewIndexer(root, st, newMockEmbedder(), NewChunker(512, 50), s, time.Time{})
			if stage == "worker" {
				s.readFile = func(string) ([]byte, error) { return nil, &PathPolicyError{Root: root, Err: os.ErrPermission} }
			} else {
				fail.Store(true)
			}
			if stage == "reconcile-cleanup" {
				_, err = idx.Reconcile(ctx, func(run func() error) error { return run() }, nil)
			} else {
				_, err = idx.IndexAll(ctx)
			}
			if !IsPathPolicyError(err) || !errors.Is(err, os.ErrPermission) {
				t.Fatalf("wanted typed policy error: %v", err)
			}
			doc, err := st.GetDocument(ctx, "gone.go")
			if err != nil || doc == nil {
				t.Fatalf("existing document lost: %v %v", doc, err)
			}
		})
	}
}

func TestPathPolicyConcurrentFactoryCapture(t *testing.T) {
	root := t.TempDir()
	p := &testPolicy{exclude: func(string) (bool, error) { return false, nil }}
	t.Cleanup(func() { SetPathPolicyFactory(nil) })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 16; j++ {
				SetPathPolicyFactory(func(string) (PathPolicy, error) { return p, nil })
				m, err := NewIgnoreMatcher(root, nil, "")
				if err != nil {
					t.Error(err)
					return
				}
				if ignored, err := m.ShouldIgnoreChecked("future.go"); err != nil || ignored {
					t.Errorf("factory capture: %v %v", ignored, err)
				}
				SetPathPolicyFactory(nil)
			}
		}()
	}
	wg.Wait()
}

func TestPathPolicyFactoryErrorAndCapture(t *testing.T) {
	SetPathPolicyFactory(func(string) (PathPolicy, error) { return nil, os.ErrPermission })
	t.Cleanup(func() { SetPathPolicyFactory(nil) })
	if _, err := NewIgnoreMatcher(t.TempDir(), nil, ""); !IsPathPolicyError(err) {
		t.Fatalf("missing factory error: %v", err)
	}
	SetPathPolicyFactory(func(string) (PathPolicy, error) {
		return &testPolicy{exclude: func(string) (bool, error) { return true, nil }}, nil
	})
	m, err := NewIgnoreMatcher(t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	SetPathPolicyFactory(nil)
	if !m.ShouldIgnore("future.go") {
		t.Fatal("existing matcher lost captured policy")
	}
	ordinary, err := NewIgnoreMatcher(t.TempDir(), nil, "")
	if err != nil || ordinary.ShouldIgnore("future.go") {
		t.Fatalf("nil factory changed ordinary behavior: %v", err)
	}
}

func TestPathPolicyGitReceiptScopeAndDynamicExclusions(t *testing.T) {
	var excluded atomic.Bool
	p := &testPolicy{scope: "selection-a", exclude: func(path string) (bool, error) { return excluded.Load() && path == "main.go", nil }}
	policyFixture(t, p)
	idx, _, _ := gitReconcileFixture(t)
	if !runGitReconcile(t, idx) {
		t.Fatal("initial walk missing")
	}
	if _, skip := idx.CanSkipReconciliation(context.Background()); !skip {
		t.Fatal("unchanged scope lost fast skip")
	}
	excluded.Store(true)
	if _, skip := idx.CanSkipReconciliation(context.Background()); skip {
		t.Fatal("dynamic exclusion reused receipt")
	}
	excluded.Store(false)
	p.scope = "selection-b"
	if _, skip := idx.CanSkipReconciliation(context.Background()); skip {
		t.Fatal("changed selection reused receipt")
	}
}

func TestScanFileRejectsQueuedIgnoredFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "excluded"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "excluded", "file.go"), []byte("package excluded"), 0644); err != nil {
		t.Fatal(err)
	}
	matcher, err := NewIgnoreMatcher(root, []string{"excluded"}, "")
	if err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(root, matcher)
	scanner.readFile = func(string) ([]byte, error) { t.Fatal("excluded queued event read source"); return nil, nil }
	file, err := scanner.ScanFile("excluded/file.go")
	if err != nil || file != nil {
		t.Fatalf("got %v, %v", file, err)
	}
}
