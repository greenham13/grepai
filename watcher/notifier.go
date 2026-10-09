package watcher

import "github.com/fsnotify/fsnotify"

// notifier is the per-process filesystem event source a Backend fans out.
// Linux and Windows use fsnotify (inotify / ReadDirectoryChangesW); macOS
// uses the directory poller in poll.go because fsnotify's kqueue backend
// opens one descriptor per watched file and a large code root exhausts the
// machine's whole open-file table (kern.maxfiles), which breaks every other
// process on the box.
type notifier interface {
	Add(path string) error
	Remove(path string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type fsnotifyNotifier struct{ fsw *fsnotify.Watcher }

func (n fsnotifyNotifier) Add(path string) error         { return n.fsw.Add(path) }
func (n fsnotifyNotifier) Remove(path string) error      { return n.fsw.Remove(path) }
func (n fsnotifyNotifier) Close() error                  { return n.fsw.Close() }
func (n fsnotifyNotifier) Events() <-chan fsnotify.Event { return n.fsw.Events }
func (n fsnotifyNotifier) Errors() <-chan error          { return n.fsw.Errors }
