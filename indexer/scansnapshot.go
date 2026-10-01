package indexer

import (
	"context"
	"encoding/gob"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yoanbernabeu/grepai/config"
)

const scanSnapshotInterval = 5 * time.Second

type scanSnapshotEntry struct {
	ModTime int64 // Nanoseconds, unlike the store's second-resolution timestamps.
	Size    int64
	Hash    string
	Chunks  int
}

// scanSnapshot is only a local optimization; the vector store remains authoritative.
// The mutex covers concurrent scan workers and serializes atomic file replacement.
type scanSnapshot struct {
	mu       sync.Mutex
	path     string
	entries  map[string]scanSnapshotEntry
	dirty    bool
	lastSave time.Time
}

func loadScanSnapshot(root string) *scanSnapshot {
	s := &scanSnapshot{path: filepath.Join(config.GetConfigDir(root), "scan-snapshot.gob"), entries: make(map[string]scanSnapshotEntry)}
	f, err := os.Open(s.path)
	if err != nil {
		log.Printf("Starting without scan snapshot %s: %v", s.path, err)
		return s
	}
	defer f.Close()
	var entries map[string]scanSnapshotEntry
	if err := gob.NewDecoder(f).Decode(&entries); err != nil {
		log.Printf("Ignoring corrupt scan snapshot %s: %v", s.path, err)
		return s
	}
	if entries != nil {
		s.entries = entries
	}
	return s
}

func (s *scanSnapshot) get(path string) (scanSnapshotEntry, bool) {
	if s == nil {
		return scanSnapshotEntry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[path]
	return entry, ok
}

func (s *scanSnapshot) record(file FileInfo, chunks int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := scanSnapshotEntry{ModTime: file.ModTimeNano, Size: file.Size, Hash: file.Hash, Chunks: chunks}
	if old, ok := s.entries[file.Path]; !ok || old != entry {
		s.entries[file.Path] = entry
		s.dirty = true
	}
}

func (s *scanSnapshot) remove(path string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[path]; ok {
		delete(s.entries, path)
		s.dirty = true
	}
}

// Reconcile even partially rebuilt stores, using the existing ListDocuments call.
func (s *scanSnapshot) reconcile(documents map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(documents) == 0 && len(s.entries) > 0 {
		log.Printf("Discarding scan snapshot %s: vector store is empty", s.path)
	}
	for path := range s.entries {
		if !documents[path] {
			delete(s.entries, path)
			s.dirty = true
		}
	}
}

func (s *scanSnapshot) save(force bool, beforeSave ...func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty || (!force && time.Since(s.lastSave) < scanSnapshotInterval) {
		return nil
	}
	// Local stores must be durable before the cache can claim these files
	// are indexed. Otherwise a crash can leave a new snapshot over an old,
	// nonempty vector store, which ListDocuments cannot detect.
	for _, persist := range beforeSave {
		if err := persist(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".scan-snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := gob.NewEncoder(f).Encode(s.entries); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	s.dirty = false
	s.lastSave = time.Now()
	return nil
}

// PersistScanSnapshot coalesces event writes; force flushes after scans and on
// periodic persistence/shutdown, including a final event inside the interval.
func (idx *Indexer) PersistScanSnapshot(ctx context.Context, force bool) error {
	if idx == nil || idx.snapshot == nil {
		return nil
	}
	if err := idx.snapshot.save(force, func() error { return idx.store.Persist(ctx) }); err != nil {
		return fmt.Errorf("persist scan snapshot: %w", err)
	}
	return nil
}
