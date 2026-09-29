package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/yoanbernabeu/grepai/indexer"
)

func newSharedTestWatcher(t *testing.T, backend *Backend, root string) *Watcher {
	t.Helper()
	ignore, err := indexer.NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatalf("NewIgnoreMatcher() error = %v", err)
	}
	w := NewWatcherWithBackend(root, ignore, 0, backend)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start(%s) error = %v", root, err)
	}
	return w
}

func awaitEvent(t *testing.T, w *Watcher, wantPath string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-w.Events():
			if ev.Path == wantPath {
				return
			}
		case err := <-w.Errors():
			t.Fatalf("watcher %s reported fatal error: %v", w.root, err)
		case <-deadline:
			t.Fatalf("watcher %s never received an event for %s", w.root, wantPath)
		}
	}
}

func expectNoEvent(t *testing.T, w *Watcher, wait time.Duration) {
	t.Helper()
	select {
	case ev := <-w.Events():
		t.Fatalf("watcher %s received unexpected event %+v", w.root, ev)
	case err := <-w.Errors():
		t.Fatalf("watcher %s reported fatal error: %v", w.root, err)
	case <-time.After(wait):
	}
}

func TestSharedBackendRoutesEventsToTheOwningRoot(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	for _, dir := range []string{rootA, rootB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := NewBackend()
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	defer backend.Close()

	a := newSharedTestWatcher(t, backend, rootA)
	b := newSharedTestWatcher(t, backend, rootB)
	defer a.Close()
	defer b.Close()

	backend.mu.RLock()
	subscribers := len(backend.subscribers)
	backend.mu.RUnlock()
	if subscribers != 2 {
		t.Fatalf("subscribers = %d, want 2 on one backend", subscribers)
	}

	if err := os.WriteFile(filepath.Join(rootA, "only-a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, a, "only-a.go")
	expectNoEvent(t, b, 200*time.Millisecond)

	if err := os.WriteFile(filepath.Join(rootB, "only-b.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, b, "only-b.go")
	expectNoEvent(t, a, 200*time.Millisecond)
}

func TestSharedBackendNestedRootReceivesEventsWithItsParent(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBackend()
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	defer backend.Close()

	outer := newSharedTestWatcher(t, backend, parent)
	inner := newSharedTestWatcher(t, backend, child)
	defer outer.Close()
	defer inner.Close()

	if err := os.WriteFile(filepath.Join(child, "shared.go"), []byte("package child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, inner, "shared.go")
	awaitEvent(t, outer, filepath.Join("child", "shared.go"))
}

func TestSharedBackendAbortedSubscriberDoesNotBlockOthers(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	for _, dir := range []string{rootA, rootB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := NewBackend()
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	defer backend.Close()

	a := newSharedTestWatcher(t, backend, rootA)
	b := newSharedTestWatcher(t, backend, rootB)
	defer b.Close()

	// Stop A without draining it, then overfill its subscriber queue so a
	// blocking send would hang the dispatcher if it ignored A's done channel.
	a.Abort()
	for i := 0; i < subscriberEventBuffer+10; i++ {
		if err := os.WriteFile(filepath.Join(rootA, "spam.go"), []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootB, "alive.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, b, "alive.go")
	if err := a.Close(); err != nil {
		t.Fatalf("Close(a) error = %v", err)
	}
	// The backend is still usable after one subscriber left.
	if err := backend.Add(rootB); err != nil {
		t.Fatalf("backend closed when a shared watcher closed: %v", err)
	}
}

func TestSharedBackendCloseEndsSubscribers(t *testing.T) {
	root := t.TempDir()
	backend, err := NewBackend()
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	w := newSharedTestWatcher(t, backend, root)
	if err := backend.Close(); err != nil {
		t.Fatalf("backend.Close() error = %v", err)
	}
	select {
	case err := <-w.Errors():
		var fatal *FatalError
		if !errors.As(err, &fatal) {
			t.Fatalf("error after backend close = %T %v, want *FatalError", err, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber did not report the closed backend")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() after backend close = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("second backend.Close() = %v", err)
	}
}

func TestOwnedBackendIsClosedWithTheWatcher(t *testing.T) {
	w := newTestWatcher(t, t.TempDir())
	if !w.ownsBackend {
		t.Fatal("NewWatcher must own its backend")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := w.backend.Add(t.TempDir()); err == nil {
		t.Fatal("owned backend still open after Close")
	}
}

func TestRootContains(t *testing.T) {
	sep := string(os.PathSeparator)
	root := sep + "code" + sep + "repo"
	cases := map[string]bool{
		root:                           true,
		root + sep + "a.go":            true,
		root + sep + "sub" + sep + "b": true,
		root + "-other" + sep + "c.go": false,
		sep + "code":                   false,
	}
	for path, want := range cases {
		if got := rootContains(root, path); got != want {
			t.Errorf("rootContains(%q, %q) = %v, want %v", root, path, got, want)
		}
	}
}

func TestSharedWatcherReleasesOnlyItsOwnWatches(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBackend()
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	defer backend.Close()

	outer := newSharedTestWatcher(t, backend, parent)
	inner := newSharedTestWatcher(t, backend, child)
	backend.mu.RLock()
	if backend.refs[child] != 2 || backend.refs[parent] != 1 {
		backend.mu.RUnlock()
		t.Fatalf("refs = %v, want child 2, parent 1", backend.refs)
	}
	backend.mu.RUnlock()

	if err := inner.Close(); err != nil {
		t.Fatalf("Close(inner) error = %v", err)
	}
	backend.mu.RLock()
	childRefs := backend.refs[child]
	backend.mu.RUnlock()
	if childRefs != 1 {
		t.Fatalf("child refs after inner close = %d, want 1 (parent still watches it)", childRefs)
	}
	// The parent still receives events under child.
	if err := os.WriteFile(filepath.Join(child, "still.go"), []byte("package child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, outer, filepath.Join("child", "still.go"))

	if err := outer.Close(); err != nil {
		t.Fatalf("Close(outer) error = %v", err)
	}
	backend.mu.RLock()
	remaining := len(backend.refs)
	backend.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("refs after both closed = %d, want 0", remaining)
	}
}

func TestSharedWatcherFailedStartReleasesPartialWatches(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBackend()
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	defer backend.Close()
	ignore, err := indexer.NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	w := NewWatcherWithBackend(root, ignore, 0, backend)
	original := w.addWatch
	w.addWatch = func(path string) error {
		if filepath.Base(path) == "b" {
			return syscall.ENOSPC
		}
		return original(path)
	}
	if err := w.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded despite ENOSPC")
	}
	backend.mu.RLock()
	remaining := len(backend.refs)
	backend.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("refs after failed Start = %d, want 0", remaining)
	}
}

func TestSubscribeToClosedBackendFailsFast(t *testing.T) {
	backend, err := NewBackend()
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ignore, err := indexer.NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	w := NewWatcherWithBackend(root, ignore, 0, backend)
	if err := w.Start(context.Background()); err == nil {
		t.Fatal("Start on a closed backend must fail at registration")
	}
}
