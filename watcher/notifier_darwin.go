//go:build darwin

package watcher

// newNotifier polls on macOS: fsnotify's kqueue backend holds an open
// descriptor for every file under every watched directory, so a 100k-file
// root takes the whole system past kern.maxfiles. FSEvents needs cgo, which
// the Pumpkin helper is built without.
func newNotifier() (notifier, error) {
	return newPollNotifier(pollIntervalFromEnv()), nil
}
