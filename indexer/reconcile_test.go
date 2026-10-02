package indexer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/store"
)

func runSnapshotGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitReconcileFixture(t *testing.T) (*Indexer, *mockStore, *atomic.Int64) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	runSnapshotGit(t, root, "init")
	runSnapshotGit(t, root, "config", "user.email", "test@example.com")
	runSnapshotGit(t, root, "config", "user.name", "Test")
	for name, text := range map[string]string{".gitignore": ".grepai/\n", "main.go": "package main\nfunc main() {}\n", "other.go": "package main\nfunc other() {}\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runSnapshotGit(t, root, "add", ".")
	runSnapshotGit(t, root, "commit", "-m", "first")
	ignore, err := NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(root, ignore)
	st := newMockStore()
	idx := NewIndexer(root, st, newMockEmbedder(), NewChunker(512, 50), scanner, time.Time{})
	calls := &atomic.Int64{}
	idx.scanMetadata = func(ctx context.Context) ([]FileMeta, []string, error) {
		calls.Add(1)
		return scanner.ScanMetadataContext(ctx)
	}
	return idx, st, calls
}

func runGitReconcile(t *testing.T, idx *Indexer) bool {
	t.Helper()
	ctx := context.Background()
	if _, skip := idx.CanSkipReconciliation(ctx); skip {
		return false
	}
	before, _ := ReadGitScanState(ctx, idx.root)
	if _, err := idx.Reconcile(ctx, func(run func() error) error { return run() }, nil); err != nil {
		t.Fatal(err)
	}
	if err := idx.CompleteGitScan(ctx, before); err != nil {
		t.Fatal(err)
	}
	return true
}

func TestReconcileGitFastPath(t *testing.T) {
	idx, st, calls := gitReconcileFixture(t)
	if !runGitReconcile(t, idx) || calls.Load() != 1 {
		t.Fatal("first start did not walk")
	}
	saved := loadScanSnapshot(idx.root).gitState
	if saved.HEAD == "" || saved.StatusHash == "" || saved.IndexModTime == 0 || saved.IndexSize == 0 || !saved.Clean {
		t.Fatalf("git state=%+v", saved)
	}
	scan := idx.scanMetadata
	idx = NewIndexer(idx.root, st, newMockEmbedder(), idx.chunker, idx.scanner, time.Now())
	idx.scanMetadata = scan
	if runGitReconcile(t, idx) || calls.Load() != 1 {
		t.Fatal("second start walked")
	}
	for _, committed := range []bool{true, false} {
		text := "package main\nfunc main() { println(1) }\n"
		if !committed {
			text = "package main\nfunc main() { println(2) }\n"
		}
		if err := os.WriteFile(filepath.Join(idx.root, "main.go"), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		if committed {
			runSnapshotGit(t, idx.root, "add", "main.go")
			runSnapshotGit(t, idx.root, "commit", "-m", "change")
		}
		if !runGitReconcile(t, idx) {
			t.Fatalf("change skipped: committed=%v", committed)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("walks=%d", calls.Load())
	}
	// A second edit to an already dirty path has the same porcelain bytes.
	state := idx.snapshot.gitState
	if err := os.WriteFile(filepath.Join(idx.root, "main.go"), []byte("package main\nfunc main() { println(3) }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	now, err := ReadGitScanState(context.Background(), idx.root)
	if err != nil {
		t.Fatal(err)
	}
	if state.StatusHash != now.StatusHash {
		t.Fatal("fixture no longer has identical dirty porcelain")
	}
	if !runGitReconcile(t, idx) {
		t.Fatal("dirty status authorized a skip")
	}
}

func TestReconcileGitFastPathInvalidation(t *testing.T) {
	for _, change := range []string{"empty-store", "partial-store", "index-stat", "scope", "missing-git", "changed-during-scan"} {
		t.Run(change, func(t *testing.T) {
			idx, st, _ := gitReconcileFixture(t)
			runGitReconcile(t, idx)
			switch change {
			case "empty-store":
				st.documents = map[string]store.Document{}
			case "partial-store":
				delete(st.documents, "main.go")
			case "index-stat":
				path := filepath.Join(idx.root, ".git", "index")
				stamp := time.Now().Add(time.Hour)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			case "scope":
				idx.SetReconciliationScope("changed chunk size")
			case "missing-git":
				t.Setenv("PATH", t.TempDir())
			case "changed-during-scan":
				before, err := ReadGitScanState(context.Background(), idx.root)
				if err != nil {
					t.Fatal(err)
				}
				idx.snapshot.remove("main.go")
				if err := os.WriteFile(filepath.Join(idx.root, "main.go"), []byte("package main\nfunc x() {}\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := idx.CompleteGitScan(context.Background(), before); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := idx.CanSkipReconciliation(context.Background()); ok {
				t.Fatal("invalid state authorized skip")
			}
		})
	}
}

func TestReconcileNonGitAlwaysWalks(t *testing.T) {
	idx, _, _ := snapshotFixture(t)
	var calls int
	idx.scanMetadata = func(ctx context.Context) ([]FileMeta, []string, error) {
		calls++
		return idx.scanner.ScanMetadataContext(ctx)
	}
	runGitReconcile(t, idx)
	runGitReconcile(t, idx)
	if calls != 2 {
		t.Fatalf("walks=%d", calls)
	}
}

func TestReconcileUsesFreshMetadataAfterLiveEvent(t *testing.T) {
	idx, st, _ := gitReconcileFixture(t)
	runGitReconcile(t, idx)
	edited := false
	_, err := idx.Reconcile(context.Background(), func(run func() error) error {
		// First operation reconciles store membership; second follows the walk.
		if !edited {
			edited = true
			return run()
		}
		path := filepath.Join(idx.root, "main.go")
		if err := os.WriteFile(path, []byte("package main\nfunc main() { println(42) }\n"), 0644); err != nil {
			return err
		}
		return run()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := idx.scanner.ScanFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if st.documents["main.go"].Hash != file.Hash {
		t.Fatal("old walk overwrote the new file")
	}
}

func TestReconcileFailureDoesNotGrantSnapshot(t *testing.T) {
	idx, _, _ := gitReconcileFixture(t)
	idx.scanMetadata = func(context.Context) ([]FileMeta, []string, error) { return nil, nil, errors.New("walk failed") }
	if _, err := idx.Reconcile(context.Background(), func(run func() error) error { return run() }, nil); err == nil {
		t.Fatal("walk failure was hidden")
	}
	if _, ok := idx.CanSkipReconciliation(context.Background()); ok {
		t.Fatal("partial scan authorized skip")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := idx.scanner.ScanMetadataContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled walk: %v", err)
	}
}
