package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/internal/fileutil"
	"github.com/yoanbernabeu/grepai/store"
	"github.com/yoanbernabeu/grepai/watcher"
)

func startDefaultWriterLockSupervisor(t *testing.T, root string, opts ...dynamicWatchSupervisorOption) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	stopped := make(chan struct{})
	opts = append([]dynamicWatchSupervisorOption{
		withWatchSupervisorDiscoverWorktrees(func(string) []string { return nil }),
	}, opts...)
	go func() {
		defer close(stopped)
		result <- runDynamicWatchSupervisor(ctx, root, &noOpEmbedder{}, opts...)
	}()
	t.Cleanup(func() {
		cancel()
		awaitWatchTestSignal(t, stopped, "default supervisor shutdown")
	})
	return cancel, result
}

func writerLockTestRoot(t *testing.T) string {
	t.Helper()
	root := canonicalPath(t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Store.Backend = "gob"
	cfg.RPG.Enabled = false
	if err := cfg.Save(root); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return root
}

func TestDefaultSupervisorWriterLockRejectsAliasBeforeStoreLoad(t *testing.T) {
	root := writerLockTestRoot(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	winnerReady := make(chan struct{})
	var winnerLoads atomic.Int32
	cancelWinner, winnerResult := startDefaultWriterLockSupervisor(t, root,
		func(cfg *dynamicWatchSupervisorConfig) {
			cfg.startupWiring.initializeStore = func(ctx context.Context, cfg *config.Config, root string) (store.VectorStore, error) {
				winnerLoads.Add(1)
				return initializeStore(ctx, cfg, root)
			}
		},
		withWatchSupervisorInitialReadyObserver(func(int) error {
			close(winnerReady)
			return nil
		}),
	)
	awaitWatchTestSignal(t, winnerReady, "winning watcher readiness")

	var loserLoads atomic.Int32
	unexpectedLoad := errors.New("losing watcher reached store load")
	var retries atomic.Int32
	_, loserResult := startDefaultWriterLockSupervisor(t, alias,
		func(cfg *dynamicWatchSupervisorConfig) {
			cfg.startupWiring.initializeStore = func(context.Context, *config.Config, string) (store.VectorStore, error) {
				loserLoads.Add(1)
				return nil, unexpectedLoad
			}
		},
		withWatchSupervisorLifecycleObserver(func(_ string, state, _ string) {
			if state == "retrying" {
				retries.Add(1)
			}
		}),
	)
	err := awaitWatchTestValue(t, loserResult, "alias lock conflict")
	var activeErr *fileutil.ProjectWriterActiveError
	if !errors.As(err, &activeErr) {
		t.Fatalf("alias supervisor error = %v, want *ProjectWriterActiveError", err)
	}
	if activeErr.ProjectRoot != root {
		t.Fatalf("conflict root = %q, want %q", activeErr.ProjectRoot, root)
	}
	if got := retries.Load(); got != 0 {
		t.Fatalf("main alias retried %d times after lock conflict", got)
	}
	if got := loserLoads.Load(); got != 0 {
		t.Fatalf("loser started store load %d times", got)
	}
	if got := winnerLoads.Load(); got != 1 {
		t.Fatalf("winner store loads = %d, want 1", got)
	}

	// A failed contender must not release the winner's lifetime lock.
	probe, err := fileutil.AcquireProjectWriterLock(alias)
	if probe != nil {
		if closeErr := probe.Close(); closeErr != nil {
			t.Errorf("close unexpected probe lock: %v", closeErr)
		}
	}
	if !errors.As(err, &activeErr) {
		t.Fatalf("winner lost lock after alias conflict: %v", err)
	}
	select {
	case err := <-winnerResult:
		t.Fatalf("winner stopped after alias conflict: %v", err)
	default:
	}

	cancelWinner()
	if err := awaitWatchTestValue(t, winnerResult, "winning watcher cancellation"); err != nil {
		t.Fatalf("winner shutdown: %v", err)
	}

	retryReady := make(chan struct{})
	var retryLoads atomic.Int32
	retryRoot := make(chan string, 1)
	cancelRetry, retryResult := startDefaultWriterLockSupervisor(t, alias,
		func(cfg *dynamicWatchSupervisorConfig) {
			cfg.startupWiring.initializeStore = func(ctx context.Context, cfg *config.Config, root string) (store.VectorStore, error) {
				retryLoads.Add(1)
				retryRoot <- root
				return initializeStore(ctx, cfg, root)
			}
		},
		withWatchSupervisorInitialReadyObserver(func(int) error {
			close(retryReady)
			return nil
		}),
	)
	awaitWatchTestSignal(t, retryReady, "alias retry readiness")
	if got := awaitWatchTestValue(t, retryRoot, "retry store root"); got != root {
		t.Fatalf("retry store root = %q, want %q", got, root)
	}
	if got := retryLoads.Load(); got != 1 {
		t.Fatalf("retry store loads = %d, want 1", got)
	}
	cancelRetry()
	if err := awaitWatchTestValue(t, retryResult, "retry cancellation"); err != nil {
		t.Fatalf("retry shutdown: %v", err)
	}
}

func TestDefaultSupervisorWriterLockReleasesAfterStartupExit(t *testing.T) {
	for _, cancelStartup := range []bool{false, true} {
		name := "error"
		if cancelStartup {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			root := writerLockTestRoot(t)
			started := make(chan struct{})
			wantErr := errors.New("store load failed")
			cancel, result := startDefaultWriterLockSupervisor(t, root,
				func(cfg *dynamicWatchSupervisorConfig) {
					cfg.startupWiring.initializeStore = func(ctx context.Context, _ *config.Config, _ string) (store.VectorStore, error) {
						close(started)
						if cancelStartup {
							<-ctx.Done()
							return nil, ctx.Err()
						}
						return nil, wantErr
					}
				},
			)
			awaitWatchTestSignal(t, started, "store load start")
			if cancelStartup {
				cancel()
			}
			err := awaitWatchTestValue(t, result, "startup exit")
			if cancelStartup && err != nil {
				t.Fatalf("canceled startup: %v", err)
			}
			if !cancelStartup && !errors.Is(err, wantErr) {
				t.Fatalf("startup error = %v, want %v", err, wantErr)
			}

			ready := make(chan struct{})
			cancelRetry, retryResult := startDefaultWriterLockSupervisor(t, root,
				withWatchSupervisorInitialReadyObserver(func(int) error {
					close(ready)
					return nil
				}),
			)
			awaitWatchTestSignal(t, ready, "watcher retry after startup exit")
			cancelRetry()
			if err := awaitWatchTestValue(t, retryResult, "retry shutdown"); err != nil {
				t.Fatalf("retry shutdown: %v", err)
			}
		})
	}
}

func TestDefaultSupervisorWriterLockPreservesMutationFence(t *testing.T) {
	root := writerLockTestRoot(t)
	fence := newWatchMutationFence()
	fatal := &watcher.FatalError{Operation: "watch", Path: root, Cause: syscall.ENOSPC}
	fence.failWithCause(fatal, nil, nil)
	var loads atomic.Int32
	_, result := startDefaultWriterLockSupervisor(t, root,
		withWatchSupervisorMutationFence(fence),
		func(cfg *dynamicWatchSupervisorConfig) {
			cfg.startupWiring.initializeStore = func(context.Context, *config.Config, string) (store.VectorStore, error) {
				loads.Add(1)
				return nil, errors.New("failed fence admitted store load")
			}
		},
	)
	if err := awaitWatchTestValue(t, result, "failed shared fence"); !errors.Is(err, fatal) {
		t.Fatalf("supervisor error = %v, want shared fence fatal %v", err, fatal)
	}
	if got := loads.Load(); got != 0 {
		t.Fatalf("failed shared fence admitted %d store loads", got)
	}
}
