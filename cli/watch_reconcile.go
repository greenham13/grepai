package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yoanbernabeu/grepai/indexer"
	"github.com/yoanbernabeu/grepai/store"
	"github.com/yoanbernabeu/grepai/watcher"
)

type reconcileOperation struct {
	run  func() error
	done chan error
}

// Each project has one writer. Live events have priority over reconciliation
// work; the walk itself and git probes run off this writer, without store locks.
func startWorkspaceProjectWriter(ctx context.Context, fence *watchMutationFence, runtime *workspaceProjectRuntime) watchMutationWorker {
	runtime.reconcileOps = make(chan reconcileOperation)
	return startWatchMutationWorker(ctx, fence, func(ctx context.Context) {
		var events <-chan watcher.FileEvent
		if runtime.watcher != nil {
			events = runtime.watcher.Events()
		}
		handle := func(event watcher.FileEvent) {
			handleFileEvent(ctx, runtime.idx, runtime.scanner, runtime.extractor,
				runtime.symbolStore, runtime.rpgEncoder, runtime.vectorStore,
				runtime.tracedLanguages, runtime.project.Path, runtime.cfg,
				&runtime.lastConfigWrite, runtime.manager, event, nil, nil, runtime.processor)
		}
		for {
			if ctx.Err() != nil {
				return
			}
			// Drain live events before admitting each low-priority operation.
			select {
			case event, ok := <-events:
				if !ok {
					events = nil
				} else {
					handle(event)
				}
				continue
			default:
			}
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					events = nil
				} else {
					handle(event)
				}
			case op := <-runtime.reconcileOps:
				// An event may have arrived alongside the request. Let it go first.
				for events != nil {
					select {
					case event, ok := <-events:
						if !ok {
							events = nil
						} else {
							handle(event)
						}
					default:
						goto drained
					}
					if ctx.Err() != nil {
						return
					}
				}
			drained:
				if ctx.Err() != nil {
					return
				}
				op.done <- op.run()
			}
		}
	})
}

func (r *workspaceProjectRuntime) applyReconcile(ctx context.Context, run func() error) error {
	op := reconcileOperation{run: run, done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r.reconcileOps <- op:
	}
	select {
	case <-ctx.Done():
		// The writer may still be inside run. The owner waits for all writers
		// before closing stores; no background writer outlives shutdown.
		return ctx.Err()
	case err := <-op.done:
		return err
	}
}

func reconcileDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		log.Printf("Warning: invalid %s=%q; using %s", name, value, fallback)
		return fallback
	}
	return duration
}

func waitReconcile(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// runStartupReconciliation is the only workspace startup walk worker. Probes
// also wait for the grace period: no git processes or index walks block ready.
func runStartupReconciliation(ctx context.Context, projects []*workspaceProjectRuntime, delay, pace time.Duration,
	skip func(context.Context, *workspaceProjectRuntime) (string, bool),
	walk func(context.Context, *workspaceProjectRuntime) error) {
	if !waitReconcile(ctx, delay) {
		return
	}
	queue := make([]*workspaceProjectRuntime, 0, len(projects))
	skipped := 0
	for _, project := range projects {
		if ctx.Err() != nil {
			return
		}
		if head, ok := skip(ctx, project); ok {
			skipped++
			if len(head) > 6 {
				head = head[:6]
			}
			log.Printf("[%s] reconciliation skipped: git state unchanged (HEAD %s)", project.project.Name, head)
		} else {
			queue = append(queue, project)
		}
	}
	log.Printf("reconciliation queued: %d project(s) need a walk, %d skipped via git state", len(queue), skipped)
	for i, project := range queue {
		if i > 0 && !waitReconcile(ctx, pace) {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err := walk(ctx, project); err != nil && ctx.Err() == nil {
			log.Printf("[%s] reconciliation failed: %v", project.project.Name, err)
		}
	}
}

// workspaceReconciliationSkipper is owned by the single queue worker. It loads
// the shared collection once, lazily after the first matching git receipt.
// Each project still checks every snapshot entry against its own document set.
func workspaceReconciliationSkipper(shared store.VectorStore) func(context.Context, *workspaceProjectRuntime) (string, bool) {
	var listed bool
	var documents []string
	var listErr error
	return func(ctx context.Context, r *workspaceProjectRuntime) (string, bool) {
		if !r.auxiliaryIndexesLoaded {
			return "", false
		}
		prefixed, ok := r.vectorStore.(*projectPrefixStore)
		if !ok {
			return r.idx.CanSkipReconciliation(ctx)
		}
		return r.idx.CanSkipReconciliationWithDocuments(ctx, func(ctx context.Context) ([]string, error) {
			if !listed {
				documents, listErr = shared.ListDocuments(ctx)
				listed = true
			}
			if listErr != nil {
				return nil, listErr
			}
			return prefixed.filterDocuments(documents), nil
		})
	}
}

func startWorkspaceReconciliation(ctx context.Context, fence *watchMutationFence, shared store.VectorStore, runtimes map[string]*workspaceProjectRuntime, staleRequests <-chan struct{}) watchMutationWorker {
	projects := make([]*workspaceProjectRuntime, 0, len(runtimes))
	for _, project := range runtimes {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].project.Name < projects[j].project.Name })
	return startWatchMutationWorker(ctx, fence, func(ctx context.Context) {
		runStartupReconciliation(ctx, projects,
			reconcileDuration("GREPAI_STARTUP_RECONCILE_DELAY", 15*time.Second),
			reconcileDuration("GREPAI_STARTUP_RECONCILE_PACE", 250*time.Millisecond),
			workspaceReconciliationSkipper(shared),
			reconcileWorkspaceProject)
		for {
			select {
			case <-ctx.Done():
				return
			case <-staleRequests:
				runStartupReconciliation(ctx, projectsWithoutLiveWatch(runtimes), 0,
					reconcileDuration("GREPAI_STARTUP_RECONCILE_PACE", 250*time.Millisecond),
					func(context.Context, *workspaceProjectRuntime) (string, bool) { return "", false },
					reconcileWorkspaceProject)
			}
		}
	})
}

func reconcileWorkspaceProject(ctx context.Context, r *workspaceProjectRuntime) error {
	before, _ := indexer.ReadGitScanState(ctx, r.project.Path)
	apply := func(run func() error) error { return r.applyReconcile(ctx, run) }
	stats, err := r.idx.Reconcile(ctx, apply, func(path string, meta *indexer.FileMeta, changed bool) error {
		if meta == nil {
			return r.symbolStore.DeleteFile(ctx, path)
		}
		if !isTracedLanguage(strings.ToLower(filepath.Ext(path)), r.tracedLanguages) {
			return nil
		}
		if !changed && !r.lastIndexTime.IsZero() && !time.Unix(meta.ModTime, 0).After(r.lastIndexTime) && r.symbolStore.IsFileIndexed(path) {
			if version, ok := r.symbolStore.GetFileExtractorVersion(path); ok && version == r.extractor.Version() {
				return nil
			}
		}
		file, err := r.scanner.ScanFile(path)
		if err != nil {
			return err
		}
		if file == nil {
			return nil
		}
		hash, hashOK := r.symbolStore.GetFileContentHash(path)
		version, versionOK := r.symbolStore.GetFileExtractorVersion(path)
		if hashOK && versionOK && hash == file.Hash && version == r.extractor.Version() {
			return nil
		}
		symbols, refs, err := extractSymbolsWithFramework(ctx, r.extractor, path, file.Content, r.processor)
		if err != nil {
			return err
		}
		return r.symbolStore.SaveFileWithSignature(ctx, path, file.Hash, r.extractor.Version(), symbols, refs)
	})
	if err != nil {
		return err
	}
	err = apply(func() error {
		if err := r.symbolStore.Persist(ctx); err != nil {
			return err
		}
		if r.rpgEncoder != nil {
			if err := r.rpgEncoder.BuildFull(ctx, r.symbolStore, r.vectorStore, nil); err != nil {
				return err
			}
			if err := r.rpgStore.Persist(ctx); err != nil {
				return err
			}
		}
		if stats.FilesIndexed > 0 || stats.ChunksCreated > 0 {
			r.cfg.Watch.LastIndexTime = time.Now()
			if err := r.cfg.Save(r.project.Path); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
		}
		return r.idx.PersistScanSnapshot(ctx, true)
	})
	if err != nil {
		return err
	}
	// A receipt is only written after vector, symbol and RPG stores succeeded.
	if err := r.idx.CompleteGitScan(ctx, before); err != nil {
		return err
	}
	log.Printf("[%s] Initial scan complete: %d files indexed, %d chunks created, %d files removed, %d skipped (took %s)",
		r.project.Name, stats.FilesIndexed, stats.ChunksCreated, stats.FilesRemoved, stats.FilesSkipped, stats.Duration.Round(time.Millisecond))
	return nil
}
