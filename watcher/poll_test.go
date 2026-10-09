package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// newTestPoller never passes on its own; tests call pass() so each assertion
// follows exactly one scan.
func newTestPoller(t *testing.T) *pollNotifier {
	t.Helper()
	n := newPollNotifier(time.Hour)
	t.Cleanup(func() { _ = n.Close() })
	return n
}

func drainPollEvents(t *testing.T, n *pollNotifier) []fsnotify.Event {
	t.Helper()
	var out []fsnotify.Event
	for {
		select {
		case ev := <-n.Events():
			out = append(out, ev)
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

func hasEvent(events []fsnotify.Event, path string, op fsnotify.Op) bool {
	for _, ev := range events {
		if ev.Name == path && ev.Has(op) {
			return true
		}
	}
	return false
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func TestPollNotifierReportsCreateWriteAndRemove(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "present.go"), "package x")
	n := newTestPoller(t)
	if err := n.Add(dir); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	n.pass()
	if events := drainPollEvents(t, n); len(events) != 0 {
		t.Fatalf("baseline entries must not be reported, got %+v", events)
	}

	file := filepath.Join(dir, "new.go")
	writeFile(t, file, "package x")
	n.pass()
	if events := drainPollEvents(t, n); !hasEvent(events, file, fsnotify.Create) || len(events) != 1 {
		t.Fatalf("expected one Create for %s, got %+v", file, events)
	}

	writeFile(t, file, "package x // longer")
	n.pass()
	if events := drainPollEvents(t, n); !hasEvent(events, file, fsnotify.Write) || len(events) != 1 {
		t.Fatalf("expected one Write for %s, got %+v", file, events)
	}

	// An in-place rewrite of the same length only moves the mtime.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	n.pass()
	if events := drainPollEvents(t, n); !hasEvent(events, file, fsnotify.Write) || len(events) != 1 {
		t.Fatalf("expected one Write after an mtime-only change, got %+v", events)
	}

	if err := os.Remove(file); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	n.pass()
	if events := drainPollEvents(t, n); !hasEvent(events, file, fsnotify.Remove) || len(events) != 1 {
		t.Fatalf("expected one Remove for %s, got %+v", file, events)
	}
	n.pass()
	if events := drainPollEvents(t, n); len(events) != 0 {
		t.Fatalf("a settled tree must stay silent, got %+v", events)
	}
}

func TestPollNotifierNewDirectoryReportsTheFilesAlreadyInsideIt(t *testing.T) {
	dir := t.TempDir()
	n := newTestPoller(t)
	if err := n.Add(dir); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	sub := filepath.Join(dir, "pkg")
	for _, d := range []string{filepath.Join(sub, "deep"), filepath.Join(sub, ".git", "objects")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	writeFile(t, filepath.Join(sub, "a.go"), "package pkg")
	writeFile(t, filepath.Join(sub, "deep", "b.go"), "package deep")
	writeFile(t, filepath.Join(sub, ".git", "objects", "c"), "blob")
	writeFile(t, filepath.Join(sub, ".hidden.go"), "package pkg")
	n.pass()
	events := drainPollEvents(t, n)
	for _, want := range []string{sub, filepath.Join(sub, "a.go"), filepath.Join(sub, "deep", "b.go")} {
		if !hasEvent(events, want, fsnotify.Create) {
			t.Errorf("missing Create for %s in %+v", want, events)
		}
	}
	for _, unwanted := range []string{filepath.Join(sub, "deep"), filepath.Join(sub, ".git", "objects", "c"), filepath.Join(sub, ".hidden.go"), filepath.Join(sub, ".git")} {
		if hasEvent(events, unwanted, fsnotify.Create) {
			t.Errorf("unexpected Create for %s in %+v", unwanted, events)
		}
	}
	if len(events) != 3 {
		t.Errorf("expected exactly 3 events, got %d: %+v", len(events), events)
	}
}

func TestPollNotifierVanishedDirectoryReportsEntriesThenItselfOnce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gone")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	file := filepath.Join(dir, "f.go")
	writeFile(t, file, "package gone")
	n := newTestPoller(t)
	if err := n.Add(dir); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	n.pass()
	events := drainPollEvents(t, n)
	if len(events) != 2 || events[0].Name != file || !events[0].Has(fsnotify.Remove) || events[1].Name != dir || !events[1].Has(fsnotify.Remove) {
		t.Fatalf("expected Remove(%s) then Remove(%s), got %+v", file, dir, events)
	}
	n.pass()
	if events := drainPollEvents(t, n); len(events) != 0 {
		t.Fatalf("a dropped directory must not report again, got %+v", events)
	}
	if err := n.Remove(dir); !errors.Is(err, fsnotify.ErrNonExistentWatch) {
		t.Fatalf("Remove() of a dropped directory = %v, want ErrNonExistentWatch", err)
	}
	if err := n.Add(dir); !os.IsNotExist(err) {
		t.Fatalf("Add() of a missing directory = %v, want not-exist", err)
	}
}

func TestPollNotifierRemoveStopsReportsAndCloseClosesChannels(t *testing.T) {
	dir := t.TempDir()
	n := newTestPoller(t)
	if err := n.Add(dir); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := n.Remove(dir); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := n.Remove(dir); !errors.Is(err, fsnotify.ErrNonExistentWatch) {
		t.Fatalf("second Remove() = %v, want ErrNonExistentWatch", err)
	}
	writeFile(t, filepath.Join(dir, "x.go"), "package x")
	n.pass()
	if events := drainPollEvents(t, n); len(events) != 0 {
		t.Fatalf("an unregistered directory must stay silent, got %+v", events)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, ok := <-n.Events(); ok {
		t.Fatal("Events() must be closed after Close()")
	}
	if _, ok := <-n.Errors(); ok {
		t.Fatal("Errors() must be closed after Close()")
	}
	if err := n.Add(dir); !errors.Is(err, fsnotify.ErrClosed) {
		t.Fatalf("Add() after Close() = %v, want ErrClosed", err)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestPollNotifierHoldsNoDescriptorsBetweenPasses(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs /proc/self/fd")
	}
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		d := filepath.Join(root, "d"+string(rune('a'+i)))
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		for j := 0; j < 20; j++ {
			writeFile(t, filepath.Join(d, "f"+string(rune('a'+j))+".go"), "package d")
		}
	}
	count := func() int {
		list, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatalf("ReadDir(/proc/self/fd) error = %v", err)
		}
		return len(list)
	}
	before := count()
	n := newTestPoller(t)
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			return n.Add(p)
		}
		return err
	}); err != nil {
		t.Fatalf("Add tree error = %v", err)
	}
	n.pass()
	if after := count(); after > before+2 {
		t.Fatalf("poller holds descriptors: %d before, %d after watching 420 entries", before, after)
	}
}

func TestPollBackendRoutesEventsToTheOwningRoot(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	for _, dir := range []string{rootA, rootB} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("Mkdir(%s) error = %v", dir, err)
		}
	}
	poller := newPollNotifier(time.Hour)
	backend := newBackendWith(poller)
	t.Cleanup(func() { _ = backend.Close() })
	wA := newSharedTestWatcher(t, backend, rootA)
	wB := newSharedTestWatcher(t, backend, rootB)
	t.Cleanup(func() { _ = wA.Close(); _ = wB.Close() })

	// A directory copied in between two passes: the watcher registers it and
	// the files inside still reach the owning root only.
	if err := os.Mkdir(filepath.Join(rootA, "pkg"), 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	writeFile(t, filepath.Join(rootA, "pkg", "one.go"), "package pkg")
	poller.pass()
	awaitEvent(t, wA, filepath.Join("pkg", "one.go"))
	expectNoEvent(t, wB, 100*time.Millisecond)

	// The registered new directory now reports its own later changes.
	writeFile(t, filepath.Join(rootA, "pkg", "one.go"), "package pkg // changed")
	poller.pass()
	awaitEvent(t, wA, filepath.Join("pkg", "one.go"))
	expectNoEvent(t, wB, 100*time.Millisecond)
}

func TestPollIntervalFromEnv(t *testing.T) {
	t.Setenv(pollIntervalEnv, "")
	if got := pollIntervalFromEnv(); got != defaultPollInterval {
		t.Fatalf("default interval = %v, want %v", got, defaultPollInterval)
	}
	t.Setenv(pollIntervalEnv, "7s")
	if got := pollIntervalFromEnv(); got != 7*time.Second {
		t.Fatalf("interval from env = %v, want 7s", got)
	}
	for _, bad := range []string{"soon", "-5s", "0"} {
		t.Setenv(pollIntervalEnv, bad)
		if got := pollIntervalFromEnv(); got != defaultPollInterval {
			t.Fatalf("interval for %q = %v, want the default", bad, got)
		}
	}
}
