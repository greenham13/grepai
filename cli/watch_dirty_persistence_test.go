package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/rpg"
	"github.com/yoanbernabeu/grepai/store"
	"github.com/yoanbernabeu/grepai/trace"
)

type dirtyRPGProbe struct {
	rpg.RPGStore
	calls atomic.Int32
	err   error
}

func (p *dirtyRPGProbe) Persist(context.Context) error { p.calls.Add(1); return p.err }

func dirtyTestRuntime(t *testing.T, p *workspacePersistence, st store.VectorStore) (*workspaceProjectRuntime, *dirtyRPGProbe) {
	t.Helper()
	root := t.TempDir()
	s := trace.NewGOBSymbolStore(filepath.Join(root, "symbols.gob"))
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	probe := &dirtyRPGProbe{}
	r := &workspaceProjectRuntime{project: config.ProjectEntry{Name: filepath.Base(root), Path: root}, symbolStore: s, rpgStore: probe,
		vectorStore: &projectPrefixStore{store: st, projectPath: root, projectName: filepath.Base(root)}}
	p.track(r)
	return r, probe
}

func TestWorkspaceIdleCancellationDoesNotPersist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newWorkspacePersistence(time.Hour)
	st := &workspaceShutdownStore{}
	done := make(chan error, 1)
	go func() {
		done <- runWorkspaceWatchLoop(&workspaceWatchLoop{ctx: ctx, store: st, fence: newWatchMutationFence(), persistence: p})
	}()
	cancel()
	if err := awaitWatchTestValue(t, done, "idle context cancellation"); err != nil {
		t.Fatal(err)
	}
	if st.persists.Load() != 0 {
		t.Fatal("idle shutdown persisted stores")
	}
}

func TestWorkspaceDirtyTimerFiresOnce(t *testing.T) {
	p := newWorkspacePersistence(time.Millisecond)
	defer p.close()
	st := &workspaceShutdownStore{}
	r, _ := dirtyTestRuntime(t, p, st)
	if err := r.symbolStore.SaveFile(context.Background(), "a.go", []trace.Symbol{{Name: "A", File: "a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	awaitWatchTestValue(t, p.ticks, "dirty timer")
	if err := p.persist(context.Background(), newWatchMutationFence(), st); err != nil {
		t.Fatal(err)
	}
	if p.timer != nil || len(p.dirty) != 0 {
		t.Fatal("timer remained armed after flush")
	}
}

func TestWorkspaceIdleTicksTouchNoProjects(t *testing.T) {
	p := newWorkspacePersistence(time.Hour)
	defer p.close()
	st := &workspaceShutdownStore{}
	_, a := dirtyTestRuntime(t, p, st)
	_, b := dirtyTestRuntime(t, p, st)
	for range 100 {
		if err := p.persist(context.Background(), newWatchMutationFence(), st); err != nil {
			t.Fatal(err)
		}
	}
	if st.persists.Load() != 0 || a.calls.Load() != 0 || b.calls.Load() != 0 {
		t.Fatal("idle tick touched stores")
	}
	if p.timer != nil {
		t.Fatal("idle timer armed")
	}
}

func TestWorkspaceMutationPersistsOnlyItsProject(t *testing.T) {
	p := newWorkspacePersistence(time.Hour)
	defer p.close()
	st := &workspaceShutdownStore{}
	r, a := dirtyTestRuntime(t, p, st)
	other, b := dirtyTestRuntime(t, p, st)
	if err := r.symbolStore.SaveFile(context.Background(), "a.go", []trace.Symbol{{Name: "A", File: "a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if p.timer == nil {
		t.Fatal("mutation did not arm timer")
	}
	if err := p.persist(context.Background(), newWatchMutationFence(), st); err != nil {
		t.Fatal(err)
	}
	if st.persists.Load() != 1 || a.calls.Load() != 1 || b.calls.Load() != 0 {
		t.Fatal("wrong projects persisted")
	}
	if _, err := os.Stat(filepath.Join(other.project.Path, "symbols.gob")); !os.IsNotExist(err) {
		t.Fatal("clean project written")
	}
	if p.timer != nil || len(p.dirty) != 0 {
		t.Fatal("clean queue kept timer")
	}
	if err := r.symbolStore.SaveFile(context.Background(), "a.go", []trace.Symbol{{Name: "A", File: "a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if p.timer != nil {
		t.Fatal("unchanged symbols armed timer")
	}
}

func TestWorkspaceVectorMutationArmsSave(t *testing.T) {
	p := newWorkspacePersistence(time.Hour)
	defer p.close()
	st := &workspaceShutdownStore{}
	r, _ := dirtyTestRuntime(t, p, st)
	if err := r.vectorStore.SaveDocument(context.Background(), store.Document{Path: "a.go"}); err != nil {
		t.Fatal(err)
	}
	if len(p.take()) != 1 {
		t.Fatal("vector mutation was not queued")
	}
}

func TestWorkspaceFailedPersistenceKeepsDirtyProject(t *testing.T) {
	p := newWorkspacePersistence(time.Hour)
	defer p.close()
	st := &workspaceShutdownStore{}
	r, a := dirtyTestRuntime(t, p, st)
	a.err = errors.New("disk full")
	p.mark(r)
	if err := p.persist(context.Background(), newWatchMutationFence(), st); err != nil {
		t.Fatal(err)
	}
	if p.timer == nil || len(p.dirty) != 1 {
		t.Fatal("failed save was lost")
	}
	a.err = nil
	if err := p.persist(context.Background(), newWatchMutationFence(), st); err != nil {
		t.Fatal(err)
	}
	if a.calls.Load() != 2 || p.timer != nil || len(p.dirty) != 0 {
		t.Fatal("retry did not drain queue")
	}
}

func TestWorkspaceChangeDuringPersistenceStaysQueued(t *testing.T) {
	p := newWorkspacePersistence(time.Hour)
	defer p.close()
	st := &workspaceShutdownStore{started: make(chan struct{}), release: make(chan struct{})}
	first, _ := dirtyTestRuntime(t, p, st)
	next, _ := dirtyTestRuntime(t, p, st)
	p.mark(first)
	done := make(chan error, 1)
	go func() { done <- p.persist(context.Background(), newWatchMutationFence(), st) }()
	awaitWatchTestSignal(t, st.started, "dirty batch persistence")
	if err := next.symbolStore.SaveFile(context.Background(), "next.go", []trace.Symbol{{Name: "Next", File: "next.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	close(st.release)
	if err := awaitWatchTestValue(t, done, "dirty persistence done"); err != nil {
		t.Fatal(err)
	}
	pending := p.take()
	if len(pending) != 1 || pending[next.project.Path] != next {
		t.Fatal("new mutation was dropped")
	}
}
