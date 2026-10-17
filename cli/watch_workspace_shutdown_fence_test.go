package cli

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/watcher"
)

type workspaceShutdownStore struct {
	mockVectorStore
	started  chan struct{}
	release  chan struct{}
	persists atomic.Int32
	closes   atomic.Int32
}

func (s *workspaceShutdownStore) Persist(context.Context) error {
	s.persists.Add(1)
	if s.started != nil {
		close(s.started)
		<-s.release
	}
	return nil
}

func (s *workspaceShutdownStore) Close() error {
	s.closes.Add(1)
	if s.started != nil {
		close(s.started)
		<-s.release
	}
	return nil
}

func TestWorkspaceGracefulPersistenceWinsFatalWithdrawalRace(t *testing.T) {
	fence := newWatchMutationFence()
	st := &workspaceShutdownStore{started: make(chan struct{}), release: make(chan struct{})}
	persistence := newWorkspacePersistence(time.Hour)
	defer persistence.close()
	r, _ := dirtyTestRuntime(t, persistence, st)
	persistence.mark(r)
	signals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	go func() {
		result <- runWorkspaceWatchLoop(&workspaceWatchLoop{
			ctx: context.Background(), store: st, runtimes: map[string]*workspaceProjectRuntime{},
			fence: fence, signals: signals, persistence: persistence,
		})
	}()
	signals <- os.Interrupt
	awaitWatchTestSignal(t, st.started, "workspace graceful persistence")

	fatal := &watcher.FatalError{Operation: "peer watch", Cause: syscall.ENOSPC}
	withdrawn := make(chan struct{})
	go fence.failWithCause(fatal, nil, func() { close(withdrawn) })
	withdrewEarly := false
	select {
	case <-withdrawn:
		withdrewEarly = true
	case <-time.After(25 * time.Millisecond):
	}
	close(st.release)
	if err := awaitWatchTestValue(t, result, "workspace graceful return"); err != nil {
		t.Fatalf("runWorkspaceWatchLoop() error = %v", err)
	}
	awaitWatchTestSignal(t, withdrawn, "workspace fatal withdrawal")
	if withdrewEarly {
		t.Fatal("fatal readiness withdrew while workspace persistence was in flight")
	}
}

func TestWorkspaceFatalWinsBeforeGracefulPersistence(t *testing.T) {
	fence := newWatchMutationFence()
	st := &workspaceShutdownStore{}
	fatal := &watcher.FatalError{Operation: "peer watch", Cause: syscall.ENOSPC}
	fence.failWithCause(fatal, nil, nil)
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	if err := runWorkspaceWatchLoop(&workspaceWatchLoop{
		ctx: context.Background(), store: st, runtimes: map[string]*workspaceProjectRuntime{},
		fence: fence, signals: signals,
	}); err != nil {
		t.Fatalf("runWorkspaceWatchLoop() error = %v", err)
	}
	if got := st.persists.Load(); got != 0 {
		t.Fatalf("workspace persisted %d times after fatal won, want 0", got)
	}
}

func TestWorkspaceStoreCloseIsFencedFromFatalWithdrawal(t *testing.T) {
	fence := newWatchMutationFence()
	st := &workspaceShutdownStore{started: make(chan struct{}), release: make(chan struct{})}
	aborted := false
	closed := make(chan struct{})
	go func() {
		closeWithMutationFence(context.Background(), fence, &aborted, st.Close)
		close(closed)
	}()
	awaitWatchTestSignal(t, st.started, "workspace store Close")

	withdrawn := make(chan struct{})
	go fence.failWithCause(&watcher.FatalError{Operation: "peer watch", Cause: syscall.ENOSPC}, nil, func() { close(withdrawn) })
	select {
	case <-withdrawn:
		t.Fatal("fatal readiness withdrew while workspace store Close was in flight")
	case <-time.After(25 * time.Millisecond):
	}
	close(st.release)
	awaitWatchTestSignal(t, closed, "workspace store Close return")
	awaitWatchTestSignal(t, withdrawn, "workspace withdrawal after Close")
	if got := st.closes.Load(); got != 1 {
		t.Fatalf("workspace store Close calls = %d, want 1", got)
	}

	fatalFirstFence := newWatchMutationFence()
	fatalFirstFence.failWithCause(&watcher.FatalError{Operation: "peer watch", Cause: syscall.ENOSPC}, nil, nil)
	skipped := &workspaceShutdownStore{}
	closeWithMutationFence(context.Background(), fatalFirstFence, &aborted, skipped.Close)
	if got := skipped.closes.Load(); got != 0 {
		t.Fatalf("workspace store closed %d times after fatal won, want 0", got)
	}
}
