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
	ctx               context.Context
	store             store.VectorStore
	runtimes          map[string]*workspaceProjectRuntime
	watchers          []watchSource
	fence             *watchMutationFence
	events            <-chan workspaceWatchEvent
	fatals            <-chan error
	signals           <-chan os.Signal
	stops             <-chan struct{}
	persistence       *workspacePersistence
	reconcileTicks    <-chan time.Time
	reconcileStale    chan<- struct{}
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
		batch := l.persistence.take()
		if len(batch) > 0 {
			persistWorkspaceOnShutdown(l.ctx, l.store, batch)
		}
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

func runWorkspaceWatchLoop(l *workspaceWatchLoop) error {
	if l.persistence == nil {
		l.persistence = newWorkspacePersistence(30 * time.Second)
	}
	defer l.persistence.close()
	for {
		select {
		case <-l.ctx.Done():
			return l.gracefulShutdown("")
		case <-l.signals:
			if !l.isBackgroundChild {
				fmt.Println("\nShutting down...")
			}
			return l.gracefulShutdown("")
		case <-l.stops:
			return l.gracefulShutdown("Stop file detected, shutting down...")
		case <-l.persistence.ticks:
			if err := l.persistence.persist(l.ctx, l.fence, l.store); err != nil {
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
		case <-l.reconcileTicks:
			select {
			case l.reconcileStale <- struct{}{}:
			default:
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
