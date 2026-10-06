package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yoanbernabeu/grepai/store"
)

// TestReconciliationStartupQueueListsSharedStoreOnce verifies a startup queue
// over N projects that all need a walk reads the shared collection exactly
// once: the git-skip checks and the walks share one listing, and nothing
// lists per project. The shared list is released once the queue finishes.
func TestReconciliationStartupQueueListsSharedStoreOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &reconcileCountingStore{VectorStore: store.NewGOBStore(filepath.Join(t.TempDir(), "shared.gob"))}
	projects := []*workspaceProjectRuntime{
		gitReconcileRuntime(t, "one", st),
		gitReconcileRuntime(t, "two", st),
		gitReconcileRuntime(t, "three", st),
	}
	// Every walk applies through the project's writer (applyReconcile sends on
	// reconcileOps). Without a running writer that send never completes, so
	// start one per project and stop them when the test ends.
	fence := newWatchMutationFence()
	for _, r := range projects {
		writer := startWorkspaceProjectWriter(ctx, fence, r)
		defer func() { cancel(); <-writer.done }()
		awaitWatchTestSignal(t, writer.started, r.project.Name+" writer start")
	}
	// Dirty every checkout so the git receipt cannot skip any walk.
	for _, r := range projects {
		if err := os.WriteFile(filepath.Join(r.project.Path, "new.go"), []byte("package main\nfunc added() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	st.lists = 0
	list := newSharedDocumentList(st)
	var walks int
	runStartupReconciliation(ctx, projects, 0, 0,
		workspaceReconciliationSkipperWithList(list),
		func(ctx context.Context, r *workspaceProjectRuntime) error {
			walks++
			return workspaceReconciliationWalker(list)(ctx, r)
		})
	if walks != len(projects) {
		t.Fatalf("walks=%d want %d", walks, len(projects))
	}
	if st.lists != 1 {
		t.Fatalf("shared document reads=%d want 1 across %d walked projects", st.lists, len(projects))
	}
	for _, r := range projects {
		docs, err := r.vectorStore.ListDocuments(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, doc := range docs {
			if doc == "new.go" {
				found = true
			}
		}
		if !found {
			t.Fatalf("[%s] new.go not indexed by the walk: %v", r.project.Name, docs)
		}
	}
	list.release()
	if list.listed || list.documents != nil {
		t.Fatal("shared list not released after the queue")
	}

	// The full queue helper releases on its own and still lists once.
	st.lists = 0
	runSharedStartupReconciliation(ctx, projects, 0, 0, st)
	if st.lists != 1 {
		t.Fatalf("runSharedStartupReconciliation reads=%d want 1", st.lists)
	}

	// A live (non-startup) reconcile with no shared list uses its own store.
	st.lists = 0
	if err := reconcileWorkspaceProject(ctx, projects[0]); err != nil {
		t.Fatal(err)
	}
	if st.lists != 1 {
		t.Fatalf("live reconcile reads=%d want 1", st.lists)
	}
}
