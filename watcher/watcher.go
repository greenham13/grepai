package watcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yoanbernabeu/grepai/indexer"
)

type EventType int

const (
	EventCreate EventType = iota
	EventModify
	EventDelete
	EventRename
)

type FileEvent struct {
	Type EventType
	Path string
}

type Watcher struct {
	root        string
	backend     *Backend
	ownsBackend bool
	// subscriberEvents/subscriberErrors are fed by the shared Backend's
	// dispatcher; backendEvents/backendErrors read from them (tests may swap
	// in their own channels).
	subscriberEvents chan fsnotify.Event
	subscriberErrors chan error
	addWatch         func(string) error
	statPath         func(string) (fs.FileInfo, error)
	relPath          func(string, string) (string, error)
	backendEvents    <-chan fsnotify.Event
	backendErrors    <-chan error
	ignore           *indexer.IgnoreMatcher
	debounceMs       int
	events           chan FileEvent
	errors           chan error
	done             chan struct{}
	stopOnce         sync.Once
	closeOnce        sync.Once
	closeErr         error
	stateMu          sync.Mutex
	ownerStopped     bool
	fatalErr         error
	fatalOnce        sync.Once

	processingDone chan struct{}

	// Debouncing state
	pending   map[string]FileEvent
	pendingMu sync.Mutex
	timer     *time.Timer
}

// NewWatcher creates a watcher with its own private fsnotify instance. Use
// NewWatcherWithBackend when several roots are watched by one process so they
// share a single instance.
func NewWatcher(root string, ignore *indexer.IgnoreMatcher, debounceMs int) (*Watcher, error) {
	backend, err := NewBackend()
	if err != nil {
		var registration *RegistrationError
		if errors.As(err, &registration) {
			registration.Path = root
		}
		return nil, err
	}
	w := NewWatcherWithBackend(root, ignore, debounceMs, backend)
	w.ownsBackend = true
	return w, nil
}

// NewWatcherWithBackend creates a watcher whose directory registrations and
// events go through a Backend shared with other watchers. Close detaches the
// watcher from the backend but leaves the backend open for the others.
func NewWatcherWithBackend(root string, ignore *indexer.IgnoreMatcher, debounceMs int, backend *Backend) *Watcher {
	w := &Watcher{
		root:             root,
		backend:          backend,
		subscriberEvents: make(chan fsnotify.Event, subscriberEventBuffer),
		subscriberErrors: make(chan error, 1),
		ignore:           ignore,
		debounceMs:       debounceMs,
		events:           make(chan FileEvent, 100),
		errors:           make(chan error, 1),
		done:             make(chan struct{}),
		pending:          make(map[string]FileEvent),
	}
	w.addWatch = backend.Add
	w.statPath = os.Stat
	w.relPath = filepath.Rel
	w.backendEvents = w.subscriberEvents
	w.backendErrors = w.subscriberErrors
	backend.subscribe(w)
	return w
}

func (w *Watcher) Start(ctx context.Context) error {
	// Add root directory and all subdirectories
	if err := w.addRecursive(w.root, true); err != nil {
		w.Abort()
		return err
	}

	// Start event processing
	w.processingDone = make(chan struct{})
	go w.processEvents(ctx)

	return nil
}

func (w *Watcher) Events() <-chan FileEvent {
	return w.events
}

// Errors returns fatal errors that stop event processing.
func (w *Watcher) Errors() <-chan error {
	return w.errors
}

// Ready invokes publish while fatal publication is excluded.
func (w *Watcher) Ready(publish func() error) error {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	if w.fatalErr != nil {
		return w.fatalErr
	}
	if w.ownerStopped {
		return errWatcherStopped
	}
	if publish == nil {
		return nil
	}
	return publish()
}

func (w *Watcher) Close() error {
	w.closeOnce.Do(func() {
		w.Abort()
		if w.backend == nil {
			return
		}
		w.backend.unsubscribe(w)
		if w.ownsBackend {
			w.closeErr = w.backend.Close()
		}
	})
	return w.closeErr
}

// Abort synchronously stops event ownership without closing the fsnotify
// backend. Fatal CLI paths rely on immediate process exit to reclaim its file
// descriptor; embedded callers may call Close after handling the fatal error.
func (w *Watcher) Abort() {
	w.stateMu.Lock()
	w.ownerStopped = true
	w.stateMu.Unlock()
	w.stop()
	w.pendingMu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.pendingMu.Unlock()
}

func (w *Watcher) stop() {
	w.stopOnce.Do(func() { close(w.done) })
}

func (w *Watcher) stopped() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// addRecursive walks the tree rooted at root and registers an fsnotify watch
// on every directory that isn't ignored. It uses filepath.WalkDir (not
// filepath.Walk) so that directory entries are read directly from the
// readdir results instead of an extra Lstat syscall per file -- on repos with
// 100k+ files this roughly halves the syscall count of the initial/restart
// tree walk, which matters because watch startup blocks on this before it
// starts serving fsnotify events.
func (w *Watcher) addRecursive(root string, rootRequired bool) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && (!rootRequired || path != root) {
				return nil
			}
			return &RegistrationError{Operation: "walk watch tree", Path: path, Cause: err}
		}

		relPath, err := w.relPath(w.root, path)
		if err != nil {
			return &RegistrationError{Operation: "resolve watch path", Path: path, Cause: err}
		}

		// Handle directories: use ShouldSkipDir to respect .grepaiignore negations
		if d.IsDir() {
			if w.ignore.ShouldSkipDir(relPath) {
				return filepath.SkipDir
			}
			// Directory is not skipped; watch it if not individually ignored
			if !w.ignore.ShouldIgnore(relPath) {
				if err := w.addWatch(path); err != nil {
					if os.IsNotExist(err) && (!rootRequired || path != root) {
						return nil
					}
					return &RegistrationError{Operation: "add watch", Path: path, Cause: err}
				}
			}
			return nil
		}

		// Skip ignored files
		if w.ignore.ShouldIgnore(relPath) {
			return nil
		}

		return nil
	})
}
