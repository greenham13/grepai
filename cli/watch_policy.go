package cli

import (
	"context"
	"github.com/yoanbernabeu/grepai/indexer"
	"github.com/yoanbernabeu/grepai/trace"
	"github.com/yoanbernabeu/grepai/watcher"
)

// Stop admission immediately; the owner waits for quiescence after the
// reporting worker returns, avoiding a worker waiting for itself.
func (f *watchMutationFence) reportWorkerError(err error) {
	f.mu.Lock()
	f.initializeLocked()
	if f.closed || f.reportedError != nil {
		f.mu.Unlock()
		return
	}
	f.reportedError = err
	cancels := make([]context.CancelCauseFunc, 0, len(f.cancellations))
	for _, cancel := range f.cancellations {
		cancels = append(cancels, cancel)
	}
	f.workerErrors <- err
	f.mu.Unlock()
	for _, cancel := range cancels {
		cancel(err)
	}
}

func (f *watchMutationFence) workerFailure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reportedError
}

func reportPolicyWalkError(fence *watchMutationFence, path string, err error) error {
	if indexer.IsPathPolicyError(err) {
		fence.reportWorkerError(&watcher.FatalError{Operation: "reconcile path policy", Path: path, Cause: err})
	}
	return err
}

// Validate the whole deletion set before removing any symbol entries.
func reconcileSymbolScope(ctx context.Context, scanner *indexer.Scanner, symbols *trace.GOBSymbolStore) error {
	var removed []string
	for _, path := range symbols.IndexedFiles() {
		keep, err := scanner.ShouldIndexChecked(path)
		if err != nil {
			return err
		}
		if !keep {
			removed = append(removed, path)
		}
	}
	for _, path := range removed {
		if err := ctx.Err(); err != nil {
			return err
		}
		keep, err := scanner.ShouldIndexChecked(path)
		if err != nil {
			return err
		}
		if !keep {
			if err := symbols.DeleteFile(ctx, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func policyWalk(fence *watchMutationFence, walk func(context.Context, *workspaceProjectRuntime) error) func(context.Context, *workspaceProjectRuntime) error {
	return func(ctx context.Context, r *workspaceProjectRuntime) error {
		return reportPolicyWalkError(fence, r.project.Path, walk(ctx, r))
	}
}
