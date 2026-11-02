package cli

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/indexer"
	"github.com/yoanbernabeu/grepai/trace"
	"github.com/yoanbernabeu/grepai/watcher"
)

type liveTestPolicy struct{ state atomic.Int32 }

func (p *liveTestPolicy) Scope() string { return "live-test" }
func (p *liveTestPolicy) Exclude(path string) (bool, error) {
	if p.state.Load() == 2 {
		return false, os.ErrPermission
	}
	return p.state.Load() == 1 && path == "main.go", nil
}
func TestPathPolicyWorkspaceWriterFailureWithdrawsReadiness(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner-error", true: "shutdown-race"}[shutdown], func(t *testing.T) {
			p := &liveTestPolicy{}
			indexer.SetPathPolicyFactory(func(string) (indexer.PathPolicy, error) { return p, nil })
			t.Cleanup(func() { indexer.SetPathPolicyFactory(nil) })
			h := newAtomicWriteHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := newFakeWatchSource()
			fence := newWatchMutationFence()
			fence.addWatcher(source)
			r := &workspaceProjectRuntime{project: config.ProjectEntry{Name: "fixture", Path: h.projectRoot}, watcher: source, idx: h.idx, scanner: h.scanner, extractor: trace.NewRegexExtractor(), symbolStore: h.symbolStore, vectorStore: h.vecStore, cfg: h.cfg}
			worker := startWorkspaceProjectWriter(ctx, fence, r)
			awaitWatchTestSignal(t, worker.started, "writer start")
			p.state.Store(2)
			select {
			case source.events <- watcher.FileEvent{Type: watcher.EventModify, Path: "main.go"}:
			case <-time.After(5 * time.Second):
				t.Fatal("writer did not receive event")
			}
			awaitWatchTestSignal(t, worker.done, "failed writer stop")
			if err := fence.handle(ctx, func(context.Context) { t.Error("admitted work after policy failure") }); !indexer.IsPathPolicyError(err) {
				t.Fatalf("admission lost failure: %v", err)
			}
			fence.cleanup(ctx, func() { t.Error("persisted after reported policy failure") })
			if shutdown {
				cancel()
			}
			withdrew := false
			err := runWorkspaceWatchLoop(&workspaceWatchLoop{ctx: ctx, store: h.vecStore, runtimes: map[string]*workspaceProjectRuntime{canonicalPath(h.projectRoot): r}, watchers: []watchSource{source}, fence: fence, workers: []watchMutationWorker{worker}, withdrawReadiness: func() { withdrew = true }})
			if !indexer.IsPathPolicyError(err) || !withdrew || source.aborted != 1 {
				t.Fatalf("fatal ownership: %v withdrew=%v aborts=%d", err, withdrew, source.aborted)
			}
			if !h.symbolStore.IsFileIndexed("main.go") {
				t.Fatal("failed writer removed symbols")
			}
		})
	}
}

func TestPathPolicyQueuedEventsPreserveDocumentsAndSymbols(t *testing.T) {
	for _, eventType := range []watcher.EventType{watcher.EventModify, watcher.EventDelete} {
		for _, state := range []int32{1, 2} {
			t.Run(eventType.String()+string(rune('0'+state)), func(t *testing.T) {
				p := &liveTestPolicy{}
				indexer.SetPathPolicyFactory(func(string) (indexer.PathPolicy, error) { return p, nil })
				t.Cleanup(func() { indexer.SetPathPolicyFactory(nil) })
				h := newAtomicWriteHarness(t)
				ctx := context.Background()
				before, err := h.vecStore.GetDocument(ctx, "main.go")
				if err != nil || before == nil {
					t.Fatal(err)
				}
				calls := h.emb.embedCalls + h.emb.embedBatchCalls
				p.state.Store(state)
				if err := os.Remove(h.srcPath); err != nil {
					t.Fatal(err)
				}
				err = handleFileEvent(ctx, h.idx, h.scanner, trace.NewRegexExtractor(), h.symbolStore, nil, h.vecStore, []string{".go"}, h.projectRoot, h.cfg, &h.lastWrite, nil, watcher.FileEvent{Type: eventType, Path: "main.go"}, nil, nil)
				if state == 2 && (!isFatalWatcherError(err) || !errors.Is(err, os.ErrPermission)) {
					t.Fatalf("missing fatal: %v", err)
				}
				if state == 1 && err != nil {
					t.Fatal(err)
				}
				after, err := h.vecStore.GetDocument(ctx, "main.go")
				if err != nil || after == nil || after.Hash != before.Hash {
					t.Fatalf("document changed: %v %v", after, err)
				}
				if !h.symbolStore.IsFileIndexed("main.go") {
					t.Fatal("symbols removed")
				}
				if calls != h.emb.embedCalls+h.emb.embedBatchCalls {
					t.Fatal("queued exclusion embedded")
				}
			})
		}
	}
}
func TestPathPolicyInitialScanReconcilesDroppedSymbols(t *testing.T) {
	p := &liveTestPolicy{}
	indexer.SetPathPolicyFactory(func(string) (indexer.PathPolicy, error) { return p, nil })
	t.Cleanup(func() { indexer.SetPathPolicyFactory(nil) })
	h := newAtomicWriteHarness(t)
	ctx := context.Background()
	p.state.Store(2)
	if _, err := runInitialScan(ctx, h.idx, h.scanner, trace.NewRegexExtractor(), h.symbolStore, []string{".go"}, time.Time{}, true, nil, nil); !indexer.IsPathPolicyError(err) {
		t.Fatalf("missing policy failure: %v", err)
	}
	if !h.symbolStore.IsFileIndexed("main.go") {
		t.Fatal("failed scan removed symbols")
	}
	p.state.Store(1)
	if _, err := runInitialScan(ctx, h.idx, h.scanner, trace.NewRegexExtractor(), h.symbolStore, []string{".go"}, time.Time{}, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if h.symbolStore.IsFileIndexed("main.go") {
		t.Fatal("dropped scope retained symbols")
	}
	if doc, err := h.vecStore.GetDocument(ctx, "main.go"); err != nil || doc != nil {
		t.Fatalf("dropped scope retained vector: %v %v", doc, err)
	}
}
