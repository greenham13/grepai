package cli

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/embedder"
	"github.com/yoanbernabeu/grepai/store"
	"github.com/yoanbernabeu/grepai/trace"
	"github.com/yoanbernabeu/grepai/watcher"
)

type fakeWatchSource struct {
	events   chan watcher.FileEvent
	errors   chan error
	closed   int
	aborted  int
	readyErr error
	readyFn  func(func() error) error
}

func newFakeWatchSource() *fakeWatchSource {
	return &fakeWatchSource{
		events: make(chan watcher.FileEvent),
		errors: make(chan error, 1),
	}
}

func (w *fakeWatchSource) Events() <-chan watcher.FileEvent { return w.events }
func (w *fakeWatchSource) Errors() <-chan error             { return w.errors }

func (w *fakeWatchSource) Ready(publish func() error) error {
	if w.readyFn != nil {
		return w.readyFn(publish)
	}
	if w.readyErr != nil {
		return w.readyErr
	}
	if publish == nil {
		return nil
	}
	return publish()
}

func (w *fakeWatchSource) Close() error {
	w.closed++
	return nil
}

func (w *fakeWatchSource) Abort() { w.aborted++ }

func TestWorkspaceReadinessRefusesPreloadedWatcherFatal(t *testing.T) {
	fatal := &watcher.FatalError{Operation: "watch", Cause: syscall.ENOSPC}
	source := newFakeWatchSource()
	source.readyErr = fatal
	published := false
	err := withWatchSourcesReady([]watchSource{source}, func() error {
		published = true
		return nil
	})
	if !errors.Is(err, fatal) {
		t.Fatalf("withWatchSourcesReady() error = %v, want fatal", err)
	}
	if published {
		t.Fatal("workspace ready callback ran after watcher fatal")
	}
}

func TestFatalWatcherErrorClassificationPreservesWrapping(t *testing.T) {
	registrationErr := &watcher.RegistrationError{Operation: "add watch", Path: "/project", Cause: syscall.EMFILE}
	if !isFatalWatcherError(errors.Join(errors.New("session failed"), registrationErr)) {
		t.Fatal("wrapped registration error was not classified as fatal")
	}
	if isFatalWatcherError(errors.New("optional initialization warning")) {
		t.Fatal("ordinary initialization error was classified as fatal")
	}
}

func TestMonitorWorkspaceWatcherFatalIdentifiesProjectAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := newFakeWatchSource()
	runtime := &workspaceProjectRuntime{
		project: config.ProjectEntry{Name: "api", Path: "/workspace/api"},
		watcher: source,
	}
	fatals := make(chan error, 1)
	fence := newWatchMutationFence()
	fence.addWatcher(source)
	mutationStarted := make(chan struct{})
	mutationCanceled := make(chan struct{})
	releaseMutation := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- fence.handle(ctx, func(eventCtx context.Context) {
			close(mutationStarted)
			<-eventCtx.Done()
			close(mutationCanceled)
			<-releaseMutation
		})
	}()
	awaitWatchTestSignal(t, mutationStarted, "workspace mutation admission")
	withdrawn := make(chan struct{})
	done := make(chan struct{})
	go func() {
		monitorWorkspaceWatcher(ctx, runtime, fence, nil, func() { close(withdrawn) }, fatals)
		close(done)
	}()
	source.errors <- &watcher.FatalError{Operation: "process filesystem events", Cause: syscall.ENOSPC}
	awaitWatchTestSignal(t, mutationCanceled, "workspace mutation cancellation")
	if err := fence.handle(ctx, func(context.Context) { t.Error("mutation admitted after workspace fatal") }); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("post-fatal handle() error = %v, want ENOSPC", err)
	}
	select {
	case err := <-fatals:
		t.Fatalf("fatal delivered before mutation quiesced: %v", err)
	default:
	}
	select {
	case <-withdrawn:
		t.Fatal("workspace readiness withdrawn before mutation quiesced")
	default:
	}
	close(releaseMutation)
	if err := awaitWatchTestValue(t, mutationDone, "workspace mutation return"); err != nil {
		t.Fatalf("workspace mutation error = %v", err)
	}

	err := awaitWatchTestValue(t, fatals, "workspace fatal delivery")
	var projectErr *workspaceWatcherError
	if !errors.As(err, &projectErr) || projectErr.ProjectName != "api" || projectErr.ProjectPath != "/workspace/api" {
		t.Fatalf("fatal error = %#v, want tagged api project", err)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("fatal error = %v, want ENOSPC", err)
	}
	awaitWatchTestSignal(t, withdrawn, "workspace readiness withdrawal")
	awaitWatchTestSignal(t, done, "workspace fatal monitor return")
}

// A project whose live watch cannot be registered (inotify limits) keeps its
// runtime and is indexed without live events; the other projects' watchers
// are untouched and the workspace keeps running.
func TestInitializeWorkspaceRuntimesRegistrationFailureKeepsIndexingWithoutLiveEvents(t *testing.T) {
	first := newNonCooperativeCloseWatchSource()
	defer close(first.blockClose)
	symbolPath := filepath.Join(t.TempDir(), "symbols.gob")
	symbolStore := trace.NewGOBSymbolStore(symbolPath)
	ws := &config.Workspace{Projects: []config.ProjectEntry{
		{Name: "first", Path: "/first"},
		{Name: "second", Path: "/second"},
		{Name: "third", Path: "/third"},
	}}
	third := newFakeWatchSource()
	initFn := func(_ context.Context, _ *config.Workspace, project config.ProjectEntry, _ embedder.Embedder, _ store.VectorStore, _ *watcher.Backend, _ bool) (*workspaceProjectRuntime, watchSource, error) {
		switch project.Name {
		case "first":
			return &workspaceProjectRuntime{project: project, watcher: first, symbolStore: symbolStore}, first, nil
		case "second":
			return &workspaceProjectRuntime{project: project}, nil, &watcher.RegistrationError{Operation: "add watch", Path: "/second", Cause: syscall.EMFILE}
		default:
			return &workspaceProjectRuntime{project: project, watcher: third}, third, nil
		}
	}

	runtimes, watchers, err := initializeWorkspaceRuntimes(context.Background(), ws, nil, nil, nil, false, initFn)
	if err != nil {
		t.Fatalf("initializeWorkspaceRuntimes() error = %v, want nil (one project without live watching is not fatal)", err)
	}
	if len(runtimes) != 3 {
		t.Fatalf("runtimes = %d, want 3 (the project without live watching is still indexed)", len(runtimes))
	}
	if len(watchers) != 2 {
		t.Fatalf("watch sources = %d, want 2", len(watchers))
	}
	if second := runtimes[canonicalPath("/second")]; second == nil || second.watcher != nil {
		t.Fatalf("degraded project runtime = %+v, want present with nil watcher", second)
	}
	if first.aborted != 0 || first.closed != 0 {
		t.Fatalf("healthy watcher aborts/closes = %d/%d, want 0/0", first.aborted, first.closed)
	}
}

// An initializer that fails before it has a runtime (store, ignore matcher)
// still only skips that project.
func TestInitializeWorkspaceRuntimesSkipsProjectWithoutRuntime(t *testing.T) {
	ws := &config.Workspace{Projects: []config.ProjectEntry{{Name: "broken", Path: "/broken"}}}
	initFn := func(context.Context, *config.Workspace, config.ProjectEntry, embedder.Embedder, store.VectorStore, *watcher.Backend, bool) (*workspaceProjectRuntime, watchSource, error) {
		return nil, nil, &watcher.RegistrationError{Operation: "add watch", Path: "/broken", Cause: syscall.ENOSPC}
	}
	runtimes, watchers, err := initializeWorkspaceRuntimes(context.Background(), ws, nil, nil, nil, false, initFn)
	if err != nil || len(runtimes) != 0 || len(watchers) != 0 {
		t.Fatalf("got runtimes=%d watchers=%d err=%v, want 0/0/nil", len(runtimes), len(watchers), err)
	}
}

func TestInitializeWorkspaceRuntimesKeepsOptionalInitializationWarningBehavior(t *testing.T) {
	ws := &config.Workspace{Projects: []config.ProjectEntry{
		{Name: "optional-failure", Path: "/optional"},
		{Name: "healthy", Path: "/healthy"},
	}}
	healthy := newFakeWatchSource()
	initFn := func(_ context.Context, _ *config.Workspace, project config.ProjectEntry, _ embedder.Embedder, _ store.VectorStore, _ *watcher.Backend, _ bool) (*workspaceProjectRuntime, watchSource, error) {
		if project.Name == "optional-failure" {
			return nil, nil, errors.New("optional index initialization failed")
		}
		return &workspaceProjectRuntime{project: project, watcher: healthy}, healthy, nil
	}

	runtimes, watchers, err := initializeWorkspaceRuntimes(context.Background(), ws, nil, nil, nil, true, initFn)
	if err != nil {
		t.Fatalf("initializeWorkspaceRuntimes() error = %v", err)
	}
	if len(runtimes) != 1 || len(watchers) != 1 {
		t.Fatalf("initialized %d runtimes and %d watchers, want 1 each", len(runtimes), len(watchers))
	}
	closeWorkspaceRuntimes(runtimes, watchers)
}
