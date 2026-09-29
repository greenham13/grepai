package watcher

import (
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// subscriberEventBuffer bounds the per-subscriber event queue between the
// shared dispatcher and a Watcher's processEvents loop.
const subscriberEventBuffer = 1024

var errBackendClosedForSubscribe = errors.New("shared filesystem watcher is closed")

// Backend owns exactly one fsnotify.Watcher (one inotify instance on Linux,
// one kqueue on BSD/macOS) and fans its events out to every Watcher that
// subscribed to it. A workspace of N projects therefore costs one inotify
// instance instead of N, which keeps it under the per-user
// fs.inotify.max_user_instances cap (128 by default) that a per-project
// fsnotify.Watcher exhausts once a workspace grows past roughly a hundred
// projects.
//
// Directory registrations are reference counted: nested roots may register
// the same directory, and it stays watched until the last of them removes it.
type Backend struct {
	fsw *fsnotify.Watcher

	mu          sync.RWMutex
	subscribers map[*Watcher]struct{}
	refs        map[string]int
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
		refs:         make(map[string]int),
		dispatchDone: make(chan struct{}),
	}
	go b.dispatch()
	return b, nil
}

// Add registers one directory with the shared fsnotify instance, counting a
// reference for the caller.
func (b *Backend) Add(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errBackendClosedForSubscribe
	}
	if b.refs[path] == 0 {
		if err := b.fsw.Add(path); err != nil {
			return err
		}
	}
	b.refs[path]++
	return nil
}

// Remove drops one reference to a directory and stops watching it when no
// subscriber still needs it.
func (b *Backend) Remove(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.refs[path] == 0 {
		return nil
	}
	b.refs[path]--
	if b.refs[path] > 0 {
		return nil
	}
	delete(b.refs, path)
	err := b.fsw.Remove(path)
	if err != nil && (errors.Is(err, fsnotify.ErrNonExistentWatch) || os.IsNotExist(err)) {
		return nil
	}
	return err
}

// subscribe attaches a watcher. On a closed backend the watcher's channels
// are closed at once so its processEvents loop reports errBackendClosed
// instead of waiting forever.
func (b *Backend) subscribe(w *Watcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(w.subscriberEvents)
		close(w.subscriberErrors)
		return
	}
	b.subscribers[w] = struct{}{}
}

func (b *Backend) unsubscribe(w *Watcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, w)
}

// snapshot returns the current subscribers without holding the lock while
// events are delivered, so subscribe/unsubscribe/Close never wait on a
// slow subscriber.
func (b *Backend) snapshot() []*Watcher {
	b.mu.RLock()
	defer b.mu.RUnlock()
	subscribers := make([]*Watcher, 0, len(b.subscribers))
	for w := range b.subscribers {
		subscribers = append(subscribers, w)
	}
	return subscribers
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
			for _, w := range b.snapshot() {
				if !rootContains(w.root, event.Name) {
					continue
				}
				select {
				case w.subscriberEvents <- event:
				case <-w.done:
				}
			}
		case err, ok := <-b.fsw.Errors:
			if !ok {
				b.closeSubscriberChannels()
				return
			}
			for _, w := range b.snapshot() {
				select {
				case w.subscriberErrors <- err:
				case <-w.done:
				default:
					// A subscriber already holds an undelivered fatal error; one is enough.
				}
			}
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
