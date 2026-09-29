package watcher

import (
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestAbortThenCloseIsSafeAndIdempotent(t *testing.T) {
	w := newTestWatcher(t, t.TempDir())
	w.timer = time.AfterFunc(time.Hour, func() {})

	w.Abort()
	w.Abort()
	if !w.ownerStopped || !w.stopped() {
		t.Fatal("Abort did not synchronously stop watcher ownership")
	}
	if w.timer != nil {
		t.Fatal("Abort did not clear debounce timer")
	}
	if err := w.backend.Add(t.TempDir()); err != nil {
		t.Fatalf("Abort closed fsnotify backend: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close after Abort failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
}

func TestAbortIsSafeConcurrentWithPublishFatalAndClose(t *testing.T) {
	w := newTestWatcher(t, t.TempDir())
	fatal := &FatalError{Operation: "test", Cause: syscall.ENOSPC}
	var calls sync.WaitGroup
	for i := 0; i < 20; i++ {
		calls.Add(3)
		go func() { defer calls.Done(); w.Abort() }()
		go func() { defer calls.Done(); w.publishFatal(fatal) }()
		go func() { defer calls.Done(); _ = w.Close() }()
	}
	calls.Wait()

	if !w.ownerStopped || !w.stopped() {
		t.Fatal("concurrent fatal lifecycle did not stop watcher")
	}
	if err := w.backend.Add(t.TempDir()); err == nil {
		t.Fatalf("Close did not close backend exactly once: %v", err)
	}
}
