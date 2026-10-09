//go:build !darwin

package watcher

import "github.com/fsnotify/fsnotify"

func newNotifier() (notifier, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return fsnotifyNotifier{fsw: fsw}, nil
}
