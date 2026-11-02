package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/yoanbernabeu/grepai/indexer"
)

type watcherTestPolicy struct{ state atomic.Int32 }

func (p *watcherTestPolicy) Scope() string { return "test" }
func (p *watcherTestPolicy) Exclude(path string) (bool, error) {
	if p.state.Load() == 2 {
		return false, os.ErrPermission
	}
	return p.state.Load() == 1 && (path == "late" || strings.HasPrefix(path, "late/")), nil
}
func TestPathPolicyPrunesNewWatchDirectoriesAndEvents(t *testing.T) {
	root := t.TempDir()
	p := &watcherTestPolicy{}
	indexer.SetPathPolicyFactory(func(string) (indexer.PathPolicy, error) { return p, nil })
	t.Cleanup(func() { indexer.SetPathPolicyFactory(nil) })
	w := newTestWatcher(t, root)
	defer w.Close()
	late := filepath.Join(root, "late")
	if err := os.MkdirAll(filepath.Join(late, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(late, ".grepaiignore"), []byte("!*"), 0644); err != nil {
		t.Fatal(err)
	}
	p.state.Store(1)
	var added []string
	w.addWatch = func(path string) error { added = append(added, path); return nil }
	if err := w.handleEvent(fsnotify.Event{Name: late, Op: fsnotify.Create}); err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("registered excluded dirs: %v", added)
	}
	if err := w.handleEvent(fsnotify.Event{Name: filepath.Join(late, "file.go"), Op: fsnotify.Write}); err != nil {
		t.Fatal(err)
	}
	if len(w.pending) != 0 {
		t.Fatal("queued excluded source")
	}
	p.state.Store(2)
	if err := w.handleEvent(fsnotify.Event{Name: filepath.Join(late, "file.go"), Op: fsnotify.Write}); !indexer.IsPathPolicyError(err) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("lost live policy error: %v", err)
	}
}
func TestPathPolicyRegistrationFailsExplicitly(t *testing.T) {
	p := &watcherTestPolicy{}
	indexer.SetPathPolicyFactory(func(string) (indexer.PathPolicy, error) { return p, nil })
	t.Cleanup(func() { indexer.SetPathPolicyFactory(nil) })
	w := newTestWatcher(t, t.TempDir())
	defer w.Close()
	p.state.Store(2)
	err := w.Start(context.Background())
	var registration *RegistrationError
	if !errors.As(err, &registration) || !indexer.IsPathPolicyError(err) {
		t.Fatalf("lost registration policy failure: %v", err)
	}
}
