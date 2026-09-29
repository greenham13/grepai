package watcher

import (
	"os"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// subscriberEventBuffer bounds the per-subscriber event queue between the
// shared dispatcher and a Watcher's processEvents loop.
const subscriberEventBuffer = 1024

// Backend owns exactly one fsnotify.Watcher (one inotify instance on Linux,
// one kqueue on BSD/macOS) and fans its events out to every Watcher that
// subscribed to it. A workspace of N projects therefore costs one inotify
// instance instead of N, which keeps it under the per-user
// fs.inotify.max_user_instances cap (128 by default) that a per-project
// fsnotify.Watcher exhausts once a workspace grows past roughly a hundred
// projects.
type Backend struct {
	fsw *fsnotify.Watcher

	mu          sync.RWMutex
	subscribers map[*Watcher]struct{}
	closed      bool

	closeOnce    sync.Once
	closeErr     error
	dispatchDone chan struct{}
}

// NewBackend creates the single fsnotify.Watcher the subscribers share.
func NewBackend() (*Backend, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, &RegistrationError{Operation: "create filesystem watcher", Path: "", Cause: err}
	}
	b := &Backend{
		fsw:          fsw,
		subscribers:  make(map[*Watcher]struct{}),
		dispatchDone: make(chan struct{}),
	}
	go b.dispatch()
	return b, nil
}

// Add registers one directory with the shared fsnotify instance.
func (b *Backend) Add(path string) error {
	return b.fsw.Add(path)
}

func (b *Backend) subscribe(w *Watcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.subscribers[w] = struct{}{}
}

func (b *Backend) unsubscribe(w *Watcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, w)
}

// dispatch is the only reader of the fsnotify channels. Each event is handed
// to every subscriber whose root contains the event path; each fsnotify error
// is handed to every subscriber. A subscriber that has stopped (its done
// channel is closed) is skipped, so one stuck or aborted watcher can never
// block delivery to the others.
func (b *Backend) dispatch() {
	defer close(b.dispatchDone)
	for {
		select {
		case event, ok := <-b.fsw.Events:
			if !ok {
				b.closeSubscriberChannels()
				return
			}
			b.mu.RLock()
			for w := range b.subscribers {
				if rootContains(w.root, event.Name) {
					select {
					case w.subscriberEvents <- event:
					case <-w.done:
					}
				}
			}
			b.mu.RUnlock()
		case err, ok := <-b.fsw.Errors:
			if !ok {
				b.closeSubscriberChannels()
				return
			}
			b.mu.RLock()
			for w := range b.subscribers {
				select {
				case w.subscriberErrors <- err:
				case <-w.done:
				default:
					// A subscriber already holds an undelivered fatal error; one is enough.
				}
			}
			b.mu.RUnlock()
		}
	}
}

// closeSubscriberChannels mirrors fsnotify closing its channels: every
// remaining subscriber sees its backend channels close and reports
// errBackendClosed unless it was stopped on purpose.
func (b *Backend) closeSubscriberChannels() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for w := range b.subscribers {
		close(w.subscriberEvents)
		close(w.subscriberErrors)
		delete(b.subscribers, w)
	}
}

// Close releases the fsnotify instance. Subscribers that are still attached
// see their event channels close, exactly as with a private fsnotify.Watcher.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.fsw.Close()
		<-b.dispatchDone
	})
	return b.closeErr
}

// rootContains reports whether path is root itself or lives underneath it.
func rootContains(root, path string) bool {
	if path == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(path, prefix)
}
