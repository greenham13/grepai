package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/indexer"
	"github.com/yoanbernabeu/grepai/store"
	"github.com/yoanbernabeu/grepai/trace"
	"github.com/yoanbernabeu/grepai/watcher"
)

type reconcileObservedStore struct {
	store.VectorStore
	saved chan string
}

func (s *reconcileObservedStore) SaveDocument(ctx context.Context, doc store.Document) error {
	if err := s.VectorStore.SaveDocument(ctx, doc); err != nil {
		return err
	}
	select {
	case s.saved <- doc.Path:
	default:
	}
	return nil
}

func reconcileRuntimeFixture(t *testing.T) (*workspaceProjectRuntime, *reconcileObservedStore, *fakeWatchSource) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ignore, err := indexer.NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	scanner := indexer.NewScanner(root, ignore)
	st := &reconcileObservedStore{VectorStore: store.NewGOBStore(config.GetIndexPath(root)), saved: make(chan string, 8)}
	source := newFakeWatchSource()
	source.events = make(chan watcher.FileEvent, 8)
	cfg := config.DefaultConfig()
	return &workspaceProjectRuntime{
		project: config.ProjectEntry{Name: "test", Path: root}, cfg: cfg,
		idx:     indexer.NewIndexer(root, st, &noOpEmbedder{}, indexer.NewChunker(512, 50), scanner, time.Time{}),
		scanner: scanner, extractor: trace.NewRegexExtractor(),
		symbolStore: trace.NewGOBSymbolStore(config.GetSymbolIndexPath(root)),
		vectorStore: st, tracedLanguages: []string{".go"}, watcher: source,
	}, st, source
}

func TestReconciliationLiveEventsDuringGrace(t *testing.T) {
	r, st, source := reconcileRuntimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fence := newWatchMutationFence()
	writer := startWorkspaceProjectWriter(ctx, fence, r)
	defer func() { cancel(); <-writer.done }()
	awaitWatchTestSignal(t, writer.started, "project writer start")
	var walks atomic.Int32
	queued := startWatchMutationWorker(ctx, fence, func(ctx context.Context) {
		runStartupReconciliation(ctx, []*workspaceProjectRuntime{r}, time.Hour, 0,
			func(context.Context, *workspaceProjectRuntime) (string, bool) { return "", false },
			func(context.Context, *workspaceProjectRuntime) error { walks.Add(1); return nil })
	})
	defer func() { cancel(); <-queued.done }()
	awaitWatchTestSignal(t, queued.started, "queue start")
	source.events <- watcher.FileEvent{Type: watcher.EventCreate, Path: "main.go"}
	if path := awaitWatchTestValue(t, st.saved, "live file indexed during grace"); path != "main.go" {
		t.Fatalf("path=%s", path)
	}
	if walks.Load() != 0 {
		t.Fatal("walk started during grace")
	}
	// Await the writer, not a timer, before reading its final symbol state.
	done := make(chan error, 1)
	go func() {
		done <- r.applyReconcile(ctx, func() error {
			if !r.symbolStore.IsFileIndexed("main.go") {
				return errors.New("live symbols missing")
			}
			return nil
		})
	}()
	if err := awaitWatchTestValue(t, done, "live symbols saved"); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationYieldsToLiveEvents(t *testing.T) {
	r, st, source := reconcileRuntimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	source.events <- watcher.FileEvent{Type: watcher.EventCreate, Path: "main.go"}
	writer := startWorkspaceProjectWriter(ctx, newWatchMutationFence(), r)
	defer func() { cancel(); <-writer.done }()
	done := make(chan error, 1)
	go func() {
		done <- r.applyReconcile(ctx, func() error {
			doc, err := st.GetDocument(ctx, "main.go")
			if err != nil {
				return err
			}
			if doc == nil {
				return errors.New("walk ran before queued live event")
			}
			return nil
		})
	}()
	if err := awaitWatchTestValue(t, done, "priority operation"); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationPacedSequentialWalks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	projects := []*workspaceProjectRuntime{
		{project: config.ProjectEntry{Name: "one"}},
		{project: config.ProjectEntry{Name: "two"}},
		{project: config.ProjectEntry{Name: "three"}},
	}
	var active atomic.Int32
	starts := make(chan string, 3)
	releases := make(chan struct{})
	done := make(chan struct{})
	const pace = 2 * time.Millisecond
	var lastEnd time.Time
	var completed atomic.Int32
	go func() {
		defer close(done)
		runStartupReconciliation(ctx, projects, 0, pace,
			func(context.Context, *workspaceProjectRuntime) (string, bool) { return "", false },
			func(ctx context.Context, r *workspaceProjectRuntime) error {
				if active.Add(1) != 1 {
					t.Error("walks overlap")
				}
				defer active.Add(-1)
				if !lastEnd.IsZero() && time.Since(lastEnd) < pace {
					t.Error("walks were not paced")
				}
				starts <- r.project.Name
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-releases:
				}
				completed.Add(1)
				lastEnd = time.Now()
				return nil
			})
	}()
	for _, want := range []string{"one", "two", "three"} {
		if got := awaitWatchTestValue(t, starts, "next walk"); got != want {
			t.Fatalf("walk=%s want %s", got, want)
		}
		if active.Load() != 1 {
			t.Fatal("no active walk")
		}
		releases <- struct{}{}
	}
	awaitWatchTestSignal(t, done, "all walks complete")
	if completed.Load() != 3 {
		t.Fatalf("walks=%d", completed.Load())
	}
}

func TestReconciliationDelayAndPaceConfig(t *testing.T) {
	for _, value := range []string{"", "bad", "-1s", "0", "12ms"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GREPAI_STARTUP_RECONCILE_DELAY", value)
			expected := 15 * time.Second
			if value == "0" {
				expected = 0
			}
			if value == "12ms" {
				expected = 12 * time.Millisecond
			}
			if got := reconcileDuration("GREPAI_STARTUP_RECONCILE_DELAY", 15*time.Second); got != expected {
				t.Fatalf("delay=%s want %s", got, expected)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitReconcile(ctx, time.Hour) {
		t.Fatal("canceled grace did not stop")
	}
}

func TestReconciliationInitializesWatchBeforeScan(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	backend, err := watcher.NewBackend()
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	shared := store.NewGOBStore(filepath.Join(t.TempDir(), "shared.gob"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := &config.Workspace{Name: "test"}
	r, w, err := initializeWorkspaceRuntime(ctx, ws, config.ProjectEntry{Name: "project", Path: root}, &noOpEmbedder{}, shared, backend, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	docs, err := shared.ListDocuments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Fatal("startup indexed files before returning the live watcher")
	}
	if _, err := os.Stat(filepath.Join(root, ".grepai", "scan-snapshot.gob")); !os.IsNotExist(err) {
		t.Fatalf("startup wrote scan snapshot: %v", err)
	}
	writer := startWorkspaceProjectWriter(ctx, newWatchMutationFence(), r)
	defer func() { cancel(); <-writer.done }()
	result := make(chan error, 1)
	go func() { result <- reconcileWorkspaceProject(ctx, r) }()
	if err := awaitWatchTestValue(t, result, "deferred scan"); err != nil {
		t.Fatal(err)
	}
	docs, err = shared.ListDocuments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0] != "test/project/main.go" {
		t.Fatalf("documents=%v", docs)
	}
}

type reconcileCountingStore struct {
	store.VectorStore
	lists   int
	listErr error
}

func (s *reconcileCountingStore) ListDocuments(ctx context.Context) ([]string, error) {
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.VectorStore.ListDocuments(ctx)
}

func gitReconcileRuntime(t *testing.T, name string, shared store.VectorStore) *workspaceProjectRuntime {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r, _, _ := reconcileRuntimeFixture(t)
	r.project.Name = name
	root := r.project.Path
	for file, text := range map[string]string{".gitignore": ".grepai/\n", "other.go": "package main\nfunc other() {}\n"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	ignore, err := indexer.NewIgnoreMatcher(root, r.cfg.Ignore, "")
	if err != nil {
		t.Fatal(err)
	}
	r.scanner = indexer.NewScanner(root, ignore)
	r.vectorStore = &projectPrefixStore{store: shared, workspaceName: "test", projectName: name, projectPath: root}
	r.idx = indexer.NewIndexer(root, r.vectorStore, &noOpEmbedder{}, indexer.NewChunker(512, 50), r.scanner, time.Time{})
	ctx := context.Background()
	before, err := indexer.ReadGitScanState(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.idx.Reconcile(ctx, func(run func() error) error { return run() }, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.idx.CompleteGitScan(ctx, before); err != nil {
		t.Fatal(err)
	}
	r.auxiliaryIndexesLoaded = true
	return r
}

func TestReconciliationSharedDocumentSnapshot(t *testing.T) {
	for _, mode := range []string{"unchanged", "partial", "empty", "error"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := &reconcileCountingStore{VectorStore: store.NewGOBStore(filepath.Join(t.TempDir(), "shared.gob"))}
			one := gitReconcileRuntime(t, "one", st)
			two := gitReconcileRuntime(t, "two", st)
			if mode == "partial" {
				if err := st.DeleteDocument(ctx, "test/two/main.go"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "empty" {
				docs, err := st.ListDocuments(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, doc := range docs {
					if err := st.DeleteDocument(ctx, doc); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "error" {
				st.listErr = errors.New("store down")
			}
			st.lists = 0
			skip := workspaceReconciliationSkipper(st)
			_, skippedOne := skip(ctx, one)
			_, skippedTwo := skip(ctx, two)
			if st.lists != 1 {
				t.Fatalf("shared document reads=%d want 1", st.lists)
			}
			if skippedOne != (mode == "unchanged" || mode == "partial") || skippedTwo != (mode == "unchanged") {
				t.Fatalf("skips one=%v two=%v mode=%s", skippedOne, skippedTwo, mode)
			}
		})
	}
}

func TestReconciliationFailedWalkKeepsQueueMoving(t *testing.T) {
	var calls int
	runStartupReconciliation(context.Background(), []*workspaceProjectRuntime{{}, {}}, 0, 0,
		func(context.Context, *workspaceProjectRuntime) (string, bool) { return "", false },
		func(context.Context, *workspaceProjectRuntime) error { calls++; return errors.New("store down") })
	if calls != 2 {
		t.Fatalf("walks=%d", calls)
	}
}
