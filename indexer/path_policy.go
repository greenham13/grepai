package indexer

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// PathPolicy can only exclude paths; false leaves ordinary ignore rules in force.
// Implementations must support concurrent calls. Scope identifies immutable policy
// settings; dynamic exclusions are rechecked before scans and git receipt reuse.
type PathPolicy interface {
	Exclude(relativePath string) (bool, error)
	Scope() string
}

type PathPolicyFactory func(projectRoot string) (PathPolicy, error)

var pathPolicyRegistration struct {
	sync.RWMutex
	factory PathPolicyFactory
}

// SetPathPolicyFactory configures subsequently constructed matchers, not existing ones.
func SetPathPolicyFactory(factory PathPolicyFactory) {
	pathPolicyRegistration.Lock()
	defer pathPolicyRegistration.Unlock()
	pathPolicyRegistration.factory = factory
}

type PathPolicyError struct {
	Root string
	Path string
	Err  error
}

func (e *PathPolicyError) Error() string {
	return fmt.Sprintf("path policy for %s (%s): %v", e.Root, e.Path, e.Err)
}
func (e *PathPolicyError) Unwrap() error { return e.Err }
func IsPathPolicyError(err error) bool {
	var policyErr *PathPolicyError
	return errors.As(err, &policyErr)
}

func newPathPolicy(root string) (PathPolicy, error) {
	pathPolicyRegistration.RLock()
	factory := pathPolicyRegistration.factory
	pathPolicyRegistration.RUnlock()
	if factory == nil {
		return nil, nil
	}
	policy, err := factory(root)
	if err != nil {
		return nil, &PathPolicyError{Root: root, Path: ".", Err: err}
	}
	return policy, nil
}

func (m *IgnoreMatcher) policyExcluded(path string) (bool, error) {
	if m == nil || m.policy == nil {
		return false, nil
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false, &PathPolicyError{Root: m.projectRoot, Path: path, Err: errors.New("path is outside index root")}
	}
	excluded, err := m.policy.Exclude(filepath.ToSlash(clean))
	if err != nil {
		return false, &PathPolicyError{Root: m.projectRoot, Path: path, Err: err}
	}
	return excluded, nil
}

func (m *IgnoreMatcher) ShouldIgnoreChecked(path string) (bool, error) {
	excluded, err := m.policyExcluded(path)
	if err != nil || excluded {
		return excluded, err
	}
	return m.shouldIgnore(path), nil
}

func (m *IgnoreMatcher) ShouldSkipDirChecked(path string) (bool, error) {
	excluded, err := m.policyExcluded(path)
	if err != nil || excluded {
		return excluded, err
	}
	return m.shouldSkipDir(path), nil
}

// Boolean compatibility methods must not admit paths when policy evaluation fails.
// Authoritative scans and mutations use the checked methods instead.
func (m *IgnoreMatcher) ShouldIgnore(path string) bool {
	excluded, err := m.ShouldIgnoreChecked(path)
	return err != nil || excluded
}
func (m *IgnoreMatcher) ShouldSkipDir(path string) bool {
	excluded, err := m.ShouldSkipDirChecked(path)
	return err != nil || excluded
}

// AllowsPath rechecks ignore rules for queued work without requiring the file to exist.
func (s *Scanner) AllowsPath(path string) (bool, error) {
	if s == nil || s.ignore == nil {
		return true, nil
	}
	excluded, err := s.ignore.ShouldIgnoreChecked(path)
	if err != nil || excluded {
		return false, err
	}
	for dir := filepath.Dir(path); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		excluded, err = s.ignore.ShouldSkipDirChecked(dir)
		if err != nil || excluded {
			return false, err
		}
	}
	return true, nil
}

func (idx *Indexer) reconciliationScope() string {
	if idx.scanner != nil && idx.scanner.ignore != nil && idx.scanner.ignore.policy != nil {
		return fmt.Sprintf("%s\x00policy:%s", idx.reconcileScope, idx.scanner.ignore.policy.Scope())
	}
	return idx.reconcileScope
}
