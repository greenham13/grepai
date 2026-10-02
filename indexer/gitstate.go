package indexer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitScanState describes the work tree at a completed scan. Dirty porcelain
// lists paths, not contents: a second edit can leave it unchanged. Such states
// are saved for diagnosis but never authorize skipping a walk.
type GitScanState struct {
	HEAD         string
	StatusHash   string
	IndexModTime int64
	IndexSize    int64
	Clean        bool
	Scope        string
}

// SetReconciliationScope binds the receipt to the caller's scan configuration.
// Set it before starting the event writer or reconciliation worker.
func (idx *Indexer) SetReconciliationScope(scope string) { idx.reconcileScope = scope }

func ReadGitScanState(ctx context.Context, root string) (GitScanState, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		// status must not refresh/write the index during this read-only probe.
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		cmd.WaitDelay = 100 * time.Millisecond
		return cmd.Output()
	}
	head, err := run("rev-parse", "--verify", "HEAD")
	if err != nil {
		return GitScanState{}, err
	}
	index, err := run("rev-parse", "--git-path", "index")
	if err != nil {
		return GitScanState{}, err
	}
	path := strings.TrimSpace(string(index))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	before, err := os.Stat(path)
	if err != nil {
		return GitScanState{}, err
	}
	status, err := run("status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		return GitScanState{}, err
	}
	after, err := os.Stat(path)
	if err != nil {
		return GitScanState{}, err
	}
	if before.ModTime() != after.ModTime() || before.Size() != after.Size() {
		return GitScanState{}, fmt.Errorf("git index changed during probe")
	}
	return GitScanState{HEAD: strings.TrimSpace(string(head)), StatusHash: fmt.Sprintf("%x", sha256.Sum256(status)), IndexModTime: after.ModTime().UnixNano(), IndexSize: after.Size(), Clean: len(status) == 0}, nil
}

// CanSkipReconciliation checks both the git receipt and the authoritative
// store. Missing documents (including an empty/rebuilt store) require a walk.
func (idx *Indexer) CanSkipReconciliation(ctx context.Context) (string, bool) {
	return idx.CanSkipReconciliationWithDocuments(ctx, idx.store.ListDocuments)
}

// CanSkipReconciliationWithDocuments permits a workspace to share one store
// list across its git checks. The supplied paths must be relative to this root.
// A stale list can only force an extra walk: live writes invalidate the receipt.
func (idx *Indexer) CanSkipReconciliationWithDocuments(ctx context.Context, list func(context.Context) ([]string, error)) (string, bool) {
	// Git does not report edits to files re-included from ignored trees.
	if idx.scanner.ignore.hasGrepaiNegations {
		return "", false
	}
	s := idx.snapshot
	s.mu.Lock()
	previous := s.gitState
	s.mu.Unlock()
	if previous.HEAD == "" || !previous.Clean {
		return "", false
	}
	current, err := ReadGitScanState(ctx, idx.root)
	current.Scope = idx.reconcileScope
	if err != nil || current != previous {
		return "", false
	}
	documents, err := list(ctx)
	if err != nil {
		return "", false
	}
	present := make(map[string]bool, len(documents))
	for _, path := range documents {
		present[path] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gitState != previous || len(documents) == 0 {
		return "", false
	}
	for path, entry := range s.entries {
		if !present[path] || entry.Chunks == 0 {
			return "", false
		}
	}
	if len(s.entries) != len(present) {
		return "", false
	}
	return current.HEAD, true
}

// CompleteGitScan only grants a receipt when git stayed stable for the whole
// scan. Callers must not call it after a partial or failed scan.
func (idx *Indexer) CompleteGitScan(ctx context.Context, before GitScanState) error {
	after, err := ReadGitScanState(ctx, idx.root)
	if err != nil || before.HEAD == "" || before != after {
		return nil
	}
	idx.snapshot.mu.Lock()
	after.Scope = idx.reconcileScope
	idx.snapshot.gitState = after
	idx.snapshot.dirty = true
	idx.snapshot.mu.Unlock()
	return idx.PersistScanSnapshot(ctx, true)
}
