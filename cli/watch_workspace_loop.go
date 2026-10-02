package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/yoanbernabeu/grepai/store"
)

type workspaceWatchLoop struct {
	ctx          context.Context
	store        store.VectorStore
	runtimes     map[string]*workspaceProjectRuntime
	watchers     []watchSource
	fence        *watchMutationFence
	events       <-chan workspaceWatchEvent
	fatals       <-chan error
	signals      <-chan os.Signal
	stops        <-chan struct{}
	persistTicks <-chan time.Time
	// staleRescanEvery: every this many persist ticks, projects without a
	// live watcher are rescanned so they do not stay stale until the next
	// watcher start. 0 disables (tests).
	reconcileStale    chan<- struct{}
	staleRescanEvery  int
	persistTickCount  int
	stopForwarders    func()
	stopWorkers       func()
	workers           []watchMutationWorker
	withdrawReadiness func()
	isBackgroundChild bool
	scope             string
}

func workspaceEventAdmissionError(runtime *workspaceProjectRuntime, err error) error {
	return &workspaceWatcherError{ProjectName: runtime.project.Name, ProjectPath: runtime.project.Path, Cause: fmt.Errorf("event admission: %w", err)}
}

func (l *workspaceWatchLoop) stopWorkersAndWait() {
	if l.stopWorkers != nil {
		l.stopWorkers()
	}
	for _, worker := range l.workers {
		<-worker.done
	}
}

func (l *workspaceWatchLoop) gracefulShutdown(message string) error {
	if message != "" {
		log.Println(message)
	}
	l.stopWorkersAndWait()
	l.fence.cleanup(l.ctx, func() {
		persistWorkspaceOnShutdown(l.ctx, l.store, l.runtimes)
	})
	return nil
}

// projectsWithoutLiveWatch lists the runtimes whose watch registration failed.
func projectsWithoutLiveWatch(runtimes map[string]*workspaceProjectRuntime) []*workspaceProjectRuntime {
	var stale []*workspaceProjectRuntime
	for _, runtime := range runtimes {
		if runtime.watcher == nil {
			stale = append(stale, runtime)
		}
	}
	return stale
}

// rescanProjectsWithoutLiveWatch re-runs the initial scan for projects that
// have no live watcher. Unchanged files are skipped by hash, so this costs a
// metadata walk per project, not a re-embedding.
func rescanProjectsWithoutLiveWatch(ctx context.Context, fence *watchMutationFence, runtimes map[string]*workspaceProjectRuntime) error {
	stale := projectsWithoutLiveWatch(runtimes)
	if len(stale) == 0 {
		return nil
	}
	return fence.handle(ctx, func(scanCtx context.Context) {
		for _, runtime := range stale {
			if runtime.idx == nil {
				continue
			}
			stats, err := runtime.idx.IndexAll(scanCtx)
			if err != nil {
				log.Printf("Warning: rescan of %s (no live file watching) failed: %v", runtime.project.Name, err)
				continue
			}
			if stats.FilesIndexed > 0 || stats.FilesRemoved > 0 {
				log.Printf("Rescanned %s (no live file watching): %d files indexed, %d removed", runtime.project.Name, stats.FilesIndexed, stats.FilesRemoved)
			}
		}
	})
}

func runWorkspaceWatchLoop(l *workspaceWatchLoop) error {
	for {
		select {
		case <-l.signals:
			if !l.isBackgroundChild {
				fmt.Println("\nShutting down...")
			}
			return l.gracefulShutdown("")
		case <-l.stops:
			return l.gracefulShutdown("Stop file detected, shutting down...")
		case <-l.persistTicks:
			if err := persistWorkspacePeriodically(l.ctx, l.fence, l.store, l.runtimes); err != nil {
				if l.ctx.Err() != nil {
					return l.gracefulShutdown("")
				}
				fatal := fmt.Errorf("workspace watcher failed during periodic persistence for %s: %w", l.scope, err)
				l.fence.failWithCause(fatal, func() { abortWatchSources(l.watchers) }, l.withdrawReadiness)
				if l.stopForwarders != nil {
					l.stopForwarders()
				}
				return fatal
			}
			l.persistTickCount++
			if l.staleRescanEvery > 0 && l.persistTickCount%l.staleRescanEvery == 0 {
				if l.reconcileStale != nil {
					select {
					case l.reconcileStale <- struct{}{}:
					default:
					}
					continue
				}
				if err := rescanProjectsWithoutLiveWatch(l.ctx, l.fence, l.runtimes); err != nil {
					if l.ctx.Err() != nil {
						return l.gracefulShutdown("")
					}
					log.Printf("Warning: rescan of projects without live file watching failed: %v", err)
				}
			}
		case err := <-l.fatals:
			if l.stopForwarders != nil {
				l.stopForwarders()
			}
			return err
		case event := <-l.events:
			runtime := l.runtimes[canonicalPath(event.projectPath)]
			if runtime == nil {
				log.Printf("Warning: received event for unknown runtime: %s", event.projectPath)
				continue
			}
			if err := l.fence.handle(l.ctx, func(eventCtx context.Context) {
				handleFileEvent(
					eventCtx, runtime.idx, runtime.scanner, runtime.extractor,
					runtime.symbolStore, runtime.rpgEncoder, runtime.vectorStore,
					runtime.tracedLanguages, runtime.project.Path, runtime.cfg,
					&runtime.lastConfigWrite, runtime.manager, event.event, nil, nil,
					runtime.processor,
				)
			}); err != nil {
				if l.ctx.Err() != nil {
					return l.gracefulShutdown("")
				}
				fatal := workspaceEventAdmissionError(runtime, err)
				l.fence.failWithCause(fatal, func() { abortWatchSources(l.watchers) }, l.withdrawReadiness)
				if l.stopForwarders != nil {
					l.stopForwarders()
				}
				return fatal
			}
		}
	}
}
