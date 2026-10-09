package watcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultPollInterval bounds how stale the index can be on a polled platform.
// A pass over a 95k-file root costs about a second of stat calls, so 30 s
// keeps the watcher at a few percent of one core.
const defaultPollInterval = 30 * time.Second

// pollIntervalEnv overrides the interval with a Go duration ("10s", "2m").
const pollIntervalEnv = "GREPAI_WATCH_POLL_INTERVAL"

func pollIntervalFromEnv() time.Duration {
	if raw := os.Getenv(pollIntervalEnv); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultPollInterval
}

type pollEntry struct {
	isDir   bool
	modTime time.Time
	size    int64
}

// pollNotifier emulates a per-directory inotify watch without holding any
// descriptor open: each registered directory keeps a snapshot of its entries
// and a single goroutine re-reads every registered directory each interval,
// reporting new, changed and vanished entries as fsnotify events. Events
// carry the same operations the fsnotify backends produce, so the Watcher
// above it is unchanged.
type pollNotifier struct {
	interval time.Duration

	mu     sync.Mutex
	dirs   map[string]map[string]pollEntry
	closed bool

	events    chan fsnotify.Event
	errors    chan error
	done      chan struct{}
	loopDone  chan struct{}
	closeOnce sync.Once
}

func newPollNotifier(interval time.Duration) *pollNotifier {
	n := &pollNotifier{
		interval: interval,
		dirs:     make(map[string]map[string]pollEntry),
		events:   make(chan fsnotify.Event, 1024),
		errors:   make(chan error, 1),
		done:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
	go n.loop()
	return n
}

func (n *pollNotifier) Events() <-chan fsnotify.Event { return n.events }
func (n *pollNotifier) Errors() <-chan error          { return n.errors }

// Add registers a directory. Its current entries are the baseline: like
// inotify, nothing already present is reported.
func (n *pollNotifier) Add(path string) error {
	path = filepath.Clean(path)
	entries, err := snapshotDir(path)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return fsnotify.ErrClosed
	}
	n.dirs[path] = entries
	return nil
}

func (n *pollNotifier) Remove(path string) error {
	path = filepath.Clean(path)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	if _, ok := n.dirs[path]; !ok {
		return fmt.Errorf("%w: %s", fsnotify.ErrNonExistentWatch, path)
	}
	delete(n.dirs, path)
	return nil
}

// Close stops the poll loop and closes both channels, as fsnotify does.
func (n *pollNotifier) Close() error {
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		n.dirs = nil
		n.mu.Unlock()
		close(n.done)
		<-n.loopDone
		close(n.events)
		close(n.errors)
	})
	return nil
}

func (n *pollNotifier) stopped() bool {
	select {
	case <-n.done:
		return true
	default:
		return false
	}
}

func (n *pollNotifier) loop() {
	defer close(n.loopDone)
	for {
		select {
		case <-n.done:
			return
		case <-time.After(n.interval):
		}
		n.pass()
	}
}

// pass scans every registered directory once. Parents sort before their
// children, so a removed tree reports top-down like the kernel backends.
func (n *pollNotifier) pass() {
	n.mu.Lock()
	paths := make([]string, 0, len(n.dirs))
	for p := range n.dirs {
		paths = append(paths, p)
	}
	n.mu.Unlock()
	sort.Strings(paths)
	for _, dir := range paths {
		if n.stopped() {
			return
		}
		n.scan(dir)
	}
}

func (n *pollNotifier) scan(dir string) {
	n.mu.Lock()
	previous, ok := n.dirs[dir]
	n.mu.Unlock()
	if !ok {
		return
	}
	current, err := snapshotDir(dir)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			// The directory is gone: its entries first, then itself (a
			// vanished root reaches the watcher's root-loss path). A watcher
			// that still holds a reference releases it through Remove later;
			// an unknown path there is tolerated.
			n.mu.Lock()
			if !n.closed {
				delete(n.dirs, dir)
			}
			n.mu.Unlock()
			for _, name := range sortedNames(previous) {
				n.emit(filepath.Join(dir, name), fsnotify.Remove)
			}
			n.emit(dir, fsnotify.Remove)
		}
		// Anything else (EACCES, a transient EMFILE) keeps the old snapshot
		// and is tried again next pass; fsnotify would stay silent too.
		return
	}
	for _, name := range sortedNames(previous) {
		before := previous[name]
		after, exists := current[name]
		full := filepath.Join(dir, name)
		switch {
		case !exists:
			n.emit(full, fsnotify.Remove)
		case before.isDir != after.isDir:
			n.emit(full, fsnotify.Remove)
			n.emitCreated(full, after)
		case !after.isDir && (!before.modTime.Equal(after.modTime) || before.size != after.size):
			n.emit(full, fsnotify.Write)
		}
	}
	for _, name := range sortedNames(current) {
		if _, existed := previous[name]; existed {
			continue
		}
		n.emitCreated(filepath.Join(dir, name), current[name])
	}
	n.mu.Lock()
	if _, still := n.dirs[dir]; still {
		n.dirs[dir] = current
	}
	n.mu.Unlock()
}

// emitCreated reports a new entry. A new directory also reports every regular
// file already inside it: those files appeared between two passes, so no
// registered directory ever saw them. The Watcher registers the new
// directories itself (Create of a directory → addRecursive) with their
// contents as the baseline, so nothing is reported twice. Dot-named entries
// are skipped: the Watcher drops them, and .git alone would be most of the walk.
func (n *pollNotifier) emitCreated(path string, entry pollEntry) {
	n.emit(path, fsnotify.Create)
	if !entry.isDir {
		return
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if n.stopped() {
			return fs.SkipAll
		}
		if err != nil || p == path {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			n.emit(p, fsnotify.Create)
		}
		return nil
	})
}

func (n *pollNotifier) emit(path string, op fsnotify.Op) {
	select {
	case n.events <- fsnotify.Event{Name: path, Op: op}:
	case <-n.done:
	}
}

// snapshotDir lists a directory without following symlinks (Info is lstat).
// An entry that vanishes between the listing and its stat is simply absent.
func snapshotDir(dir string) (map[string]pollEntry, error) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]pollEntry, len(list))
	for _, d := range list {
		info, err := d.Info()
		if err != nil {
			continue
		}
		entries[d.Name()] = pollEntry{isDir: info.IsDir(), modTime: info.ModTime(), size: info.Size()}
	}
	return entries, nil
}

func sortedNames(entries map[string]pollEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
