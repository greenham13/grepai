package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/indexer"
)

type reviewPolicy struct{ exclude func(string) (bool, error) }

func (p reviewPolicy) Scope() string                     { return "review" }
func (p reviewPolicy) Exclude(path string) (bool, error) { return p.exclude(path) }

func TestReviewDisappearingFileMustNotBecomeFatalPolicyFailure(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "reconcile"}[strict], func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "vanishing.go")
			if err := os.WriteFile(file, []byte("package fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			armed := false
			indexer.SetPathPolicyFactory(func(string) (indexer.PathPolicy, error) {
				return reviewPolicy{func(path string) (bool, error) {
					if armed && path == "vanishing.go" {
						if err := os.Remove(file); err != nil {
							t.Fatal(err)
						}
						armed = false
					}
					return false, nil
				}}, nil
			})
			defer indexer.SetPathPolicyFactory(nil)
			matcher, err := indexer.NewIgnoreMatcher(root, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			armed = true
			scanner := indexer.NewScanner(root, matcher)
			if strict {
				_, _, err = scanner.ScanMetadataContext(context.Background())
			} else {
				_, _, err = scanner.ScanMetadata()
			}
			if err != nil {
				t.Fatalf("ordinary concurrent file deletion failed scan: %v", err)
			}
		})
	}
}

func TestReviewReconcilePolicyFailureSurvivesGracefulCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fence := newWatchMutationFence()
	runtime := &workspaceProjectRuntime{project: config.ProjectEntry{Name: "fixture", Path: t.TempDir()}}
	worker := startWorkspaceProjectWriter(ctx, fence, runtime)
	awaitWatchTestSignal(t, worker.started, "writer")
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	policyErr := &indexer.PathPolicyError{Root: runtime.project.Path, Path: "file.go", Err: os.ErrPermission}
	go func() {
		err := runtime.applyReconcile(ctx, func() error { close(entered); <-release; return policyErr })
		result <- reportPolicyWalkError(fence, runtime.project.Path, err)
	}()
	awaitWatchTestSignal(t, entered, "operation")
	cancel()
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not cancel")
	}
	close(release)
	awaitWatchTestSignal(t, worker.done, "writer stop")
	if !indexer.IsPathPolicyError(fence.workerFailure()) {
		t.Fatalf("fatal operation error was lost during graceful cancellation: workerFailure=%v", fence.workerFailure())
	}
	persisted := false
	fence.cleanup(ctx, func() { persisted = true })
	if persisted {
		t.Fatal("cleanup persisted after late policy failure")
	}
	withdrew := false
	err := runWorkspaceWatchLoop(&workspaceWatchLoop{ctx: ctx, fence: fence, workers: []watchMutationWorker{worker}, withdrawReadiness: func() { withdrew = true }})
	if !indexer.IsPathPolicyError(err) || !withdrew {
		t.Fatalf("shutdown lost failure: %v, withdrew=%v", err, withdrew)
	}
}
