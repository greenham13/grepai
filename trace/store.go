package trace

import (
	"context"
	"crypto/rand"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/yoanbernabeu/grepai/internal/fileutil"
)

// GOBSymbolStore implements SymbolStore using GOB encoding.
//
// On disk the store is a base snapshot (symbols.gob, the format every grepai
// release reads) plus an append-only change log (symbols.gob.log, see
// store_log.go). Persist appends the files that changed since the last
// persist to the log and rewrites the base only when the log has grown past
// a fraction of the base, so a one-file change no longer rewrites the whole
// snapshot. A quiet store also merges after ten minutes, through the
// watcher's periodic Persist calls, so older readers see recent changes.
//
// In memory every symbol, reference and call edge is grouped by the file
// named in its File field, and each file remembers which names it
// contributed. Replacing or deleting a file therefore touches only that
// file's entries, not the whole store.
type GOBSymbolStore struct {
	indexPath string
	lockPath  string
	logPath   string

	symbols      map[string]map[string][]Symbol    // name -> file -> symbols
	refs         map[string]map[string][]Reference // symbol name -> file -> references
	edges        map[string][]CallEdge             // file -> call edges
	fileSymNames map[string][]string               // file -> distinct symbol names it defines
	fileRefNames map[string][]string               // file -> distinct names it references
	totalSymbols int
	totalRefs    int
	updatedAt    time.Time
	version      int

	fileIndex             map[string]bool
	fileContentHashes     map[string]string
	fileExtractorVersions map[string]string

	constructorPersistPending bool
	mutationGeneration        uint64
	// dirty maps a file to true when its entries changed and false when
	// only its hash/version metadata changed since the last persist.
	dirty map[string]bool

	// logID ties the change log to the base it extends. Empty means the
	// base on disk (if any) has no log identity and the next persist must
	// rewrite it.
	logID string
	// logSize is the length of the valid log prefix; 0 means the log must
	// be (re)created with a fresh header before the next append.
	logSize   int64
	oldestLog time.Time
	lastSaved time.Time
	now       func() time.Time
	// baseStamp identifies the base file this store last read or wrote, so
	// a base replaced by another process is never extended with our log.
	baseStamp fileStamp

	// removalVisits counts entries examined while removing file groups.
	// It exists so tests can prove a per-file update stays per-file.
	removalVisits uint64

	edgeCacheMu    sync.Mutex
	edgeCache      []CallEdge
	edgeCacheValid bool

	mu sync.RWMutex
}

type gobSymbolData struct {
	Index             SymbolIndex
	FileIndex         map[string]bool
	FileContentHashes map[string]string
	// FileExtractorVersions records the extractor.Version() value used
	// when each file's symbols were extracted. Pairing this with the
	// content hash lets dedup invalidate cached extractions when a new
	// release ships better symbol coverage even though the source files
	// themselves haven't changed. Older gob files without this field
	// decode with a nil map; the dedup check then treats every file as
	// "extractor version unknown" and re-extracts once, which is the
	// correct behavior for a one-time upgrade.
	FileExtractorVersions map[string]string
	// LogID names the change log that extends this base. Releases that do
	// not know the field ignore it on decode and drop it on write, which
	// orphans the log: a log whose header does not match is never replayed.
	LogID string
}

type fileStamp struct {
	size    int64
	modTime time.Time
	valid   bool
}

func statStamp(info os.FileInfo) fileStamp {
	return fileStamp{size: info.Size(), modTime: info.ModTime(), valid: true}
}

// NewGOBSymbolStore creates a new GOB-based symbol store.
func NewGOBSymbolStore(indexPath string) *GOBSymbolStore {
	s := &GOBSymbolStore{
		indexPath:                 indexPath,
		lockPath:                  indexPath + ".lock",
		logPath:                   indexPath + ".log",
		now:                       time.Now,
		constructorPersistPending: true,
	}
	s.resetUnlocked()
	return s
}

func (s *GOBSymbolStore) resetUnlocked() {
	s.symbols = make(map[string]map[string][]Symbol)
	s.refs = make(map[string]map[string][]Reference)
	s.edges = make(map[string][]CallEdge)
	s.fileSymNames = make(map[string][]string)
	s.fileRefNames = make(map[string][]string)
	s.totalSymbols = 0
	s.totalRefs = 0
	s.updatedAt = time.Time{}
	s.version = 1
	s.fileIndex = make(map[string]bool)
	s.fileContentHashes = make(map[string]string)
	s.fileExtractorVersions = make(map[string]string)
	s.dirty = make(map[string]bool)
	s.logID = ""
	s.logSize = 0
	s.oldestLog = time.Time{}
	s.lastSaved = time.Time{}
	s.baseStamp = fileStamp{}
	s.invalidateEdgeCache()
}

// invalidateEdgeCache must be called with s.mu held for writing.
func (s *GOBSymbolStore) invalidateEdgeCache() {
	s.edgeCache = nil
	s.edgeCacheValid = false
}

// Load reads the index from storage.
func (s *GOBSymbolStore) Load(ctx context.Context) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded := false
	defer func() {
		if err == nil {
			s.constructorPersistPending = false
			if loaded {
				s.mutationGeneration = 0
			}
		}
	}()

	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		loaded, err = s.loadUnlocked()
		return err
	}
	defer lockFile.Close()
	if err := fileutil.FlockShared(lockFile, false); err != nil {
		loaded, err = s.loadUnlocked()
		return err
	}
	defer func() {
		_ = fileutil.Funlock(lockFile)
	}()

	loaded, err = s.loadUnlocked()
	return err
}

func (s *GOBSymbolStore) loadUnlocked() (bool, error) {
	file, err := os.Open(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to open symbol index: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("failed to stat symbol index: %w", err)
	}

	var data gobSymbolData
	if err := gob.NewDecoder(file).Decode(&data); err != nil {
		return false, fmt.Errorf("failed to decode symbol index: %w", err)
	}

	s.resetUnlocked()
	s.applyBaseUnlocked(&data)
	s.baseStamp = statStamp(info)
	s.logID = data.LogID
	if s.logID != "" {
		if err := s.replayLogUnlocked(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// applyBaseUnlocked distributes a decoded snapshot into per-file groups.
func (s *GOBSymbolStore) applyBaseUnlocked(data *gobSymbolData) {
	for name, list := range data.Index.Symbols {
		s.symbols[name] = groupFileEntries(list, func(v Symbol) string { return v.File })
		s.totalSymbols += len(list)
		for file := range s.symbols[name] {
			s.fileSymNames[file] = append(s.fileSymNames[file], name)
		}
	}
	for name, list := range data.Index.References {
		s.refs[name] = groupFileEntries(list, func(v Reference) string { return v.File })
		s.totalRefs += len(list)
		for file := range s.refs[name] {
			s.fileRefNames[file] = append(s.fileRefNames[file], name)
		}
	}
	s.edges = groupFileEntries(data.Index.CallGraph, func(v CallEdge) string { return v.File })
	s.updatedAt = data.Index.UpdatedAt
	if data.Index.Version != 0 {
		s.version = data.Index.Version
	}
	if data.FileIndex != nil {
		s.fileIndex = data.FileIndex
	}
	if data.FileContentHashes != nil {
		s.fileContentHashes = data.FileContentHashes
	}
	if data.FileExtractorVersions != nil {
		s.fileExtractorVersions = data.FileExtractorVersions
	}
}

func (s *GOBSymbolStore) appendSymbolUnlocked(name string, sym Symbol) {
	byFile := s.symbols[name]
	if byFile == nil {
		byFile = make(map[string][]Symbol)
		s.symbols[name] = byFile
	}
	list, ok := byFile[sym.File]
	if !ok {
		s.fileSymNames[sym.File] = append(s.fileSymNames[sym.File], name)
	}
	byFile[sym.File] = append(list, sym)
	s.totalSymbols++
}

func (s *GOBSymbolStore) appendRefUnlocked(name string, ref Reference) {
	byFile := s.refs[name]
	if byFile == nil {
		byFile = make(map[string][]Reference)
		s.refs[name] = byFile
	}
	list, ok := byFile[ref.File]
	if !ok {
		s.fileRefNames[ref.File] = append(s.fileRefNames[ref.File], name)
	}
	byFile[ref.File] = append(list, ref)
	s.totalRefs++
}

// Persist writes the index to storage.
func (s *GOBSymbolStore) Persist(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.constructorPersistPending && len(s.dirty) == 0 && !s.idleMergeDueUnlocked() {
		return nil
	}

	if err := fileutil.EnsureParentDir(s.indexPath); err != nil {
		return fmt.Errorf("failed to prepare symbol index directory: %w", err)
	}

	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open symbol lock: %w", err)
	}
	defer lockFile.Close()
	if err := fileutil.FlockExclusive(lockFile, false); err != nil {
		return fmt.Errorf("failed to lock symbol index: %w", err)
	}
	defer func() {
		_ = fileutil.Funlock(lockFile)
	}()

	return s.persistUnlocked()
}

func (s *GOBSymbolStore) persistUnlocked() error {
	now := s.now()
	if s.idleMergeDueUnlocked() {
		if !s.canAppendUnlocked() {
			if err := s.reloadForMergeUnlocked(); err != nil {
				return err
			}
		}
		return s.compactUnlocked(now)
	}
	canAppend := s.canAppendUnlocked()
	if canAppend {
		batch, err := s.encodeDirtyBatchUnlocked(now)
		if err != nil {
			return err
		}
		if !s.logTooLargeUnlocked(int64(len(batch))) {
			if err := s.appendLogUnlocked(batch); err != nil {
				return err
			}
			s.updatedAt = now
			s.finishPersistUnlocked()
			return nil
		}
	}
	if !canAppend && s.baseStamp.valid && s.logID != "" {
		if err := s.reloadForMergeUnlocked(); err != nil {
			return err
		}
	}
	return s.compactUnlocked(now)
}

func (s *GOBSymbolStore) idleMergeDueUnlocked() bool {
	return len(s.dirty) == 0 && !s.oldestLog.IsZero() && s.now().Sub(s.oldestLog) >= 10*time.Minute && s.now().Sub(s.lastSaved) >= time.Minute
}

// canAppendUnlocked reports whether the base on disk is the one our log
// extends. Anything else -- a fresh store, a base from a release without
// log support, a base replaced or removed by another process -- needs a
// full rewrite.
func (s *GOBSymbolStore) canAppendUnlocked() bool {
	if s.constructorPersistPending || s.logID == "" || !s.baseStamp.valid {
		return false
	}
	info, err := os.Stat(s.indexPath)
	if err != nil {
		return false
	}
	current := statStamp(info)
	return current.size == s.baseStamp.size && current.modTime.Equal(s.baseStamp.modTime) && s.logMatchesUnlocked()
}

func (s *GOBSymbolStore) finishPersistUnlocked() {
	s.constructorPersistPending = false
	s.mutationGeneration = 0
	s.dirty = make(map[string]bool)
}

// compactUnlocked rewrites the base snapshot with a new log identity and
// drops the log it supersedes.
func (s *GOBSymbolStore) compactUnlocked(now time.Time) error {
	defer s.invalidateEdgeCache()
	logID, err := newLogID()
	if err != nil {
		return err
	}
	data := gobSymbolData{
		Index: SymbolIndex{
			Symbols:    s.flatSymbolsUnlocked(),
			References: s.flatRefsUnlocked(),
			CallGraph:  s.allEdgesLocked(),
			UpdatedAt:  now,
			Version:    s.version,
		},
		FileIndex:             s.fileIndex,
		FileContentHashes:     s.fileContentHashes,
		FileExtractorVersions: s.fileExtractorVersions,
		LogID:                 logID,
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(s.indexPath), filepath.Base(s.indexPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create symbol index temp file: %w", err)
	}

	tmpPath := tmpFile.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := gob.NewEncoder(tmpFile).Encode(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to encode symbol index: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to sync symbol index temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close symbol index temp file: %w", err)
	}
	if err := fileutil.ReplaceFileAtomically(tmpPath, s.indexPath); err != nil {
		return fmt.Errorf("failed to replace symbol index file: %w", err)
	}
	cleanupTemp = false
	info, err := os.Stat(s.indexPath)
	if err != nil {
		// The new base is in place; without its stamp the next persist
		// rewrites it again rather than extending it blindly.
		s.baseStamp = fileStamp{}
	} else {
		s.baseStamp = statStamp(info)
	}
	s.logID = logID
	// The old log names the previous base, so it can never be replayed on
	// this one. Removing it only reclaims space; if removal fails the next
	// append truncates it (logSize 0 forces a fresh header).
	_ = os.Remove(s.logPath)
	s.logSize = 0
	s.oldestLog = time.Time{}
	s.updatedAt = now
	s.finishPersistUnlocked()
	return nil
}

func newLogID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("failed to create symbol log id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// flatSymbolsUnlocked builds the snapshot's name -> symbols map with files
// in sorted order, so the base is deterministic for identical content.
func (s *GOBSymbolStore) flatSymbolsUnlocked() map[string][]Symbol {
	out := make(map[string][]Symbol, len(s.symbols))
	for name, byFile := range s.symbols {
		out[name] = flattenByFile(byFile)
	}
	return out
}

func (s *GOBSymbolStore) flatRefsUnlocked() map[string][]Reference {
	out := make(map[string][]Reference, len(s.refs))
	for name, byFile := range s.refs {
		out[name] = flattenByFile(byFile)
	}
	return out
}

// flattenByFile concatenates per-file groups in file order. A single group
// is returned as is; callers that hand the result out must copy it.
func flattenByFile[T any](byFile map[string][]T) []T {
	if len(byFile) == 1 {
		for _, list := range byFile {
			return list
		}
	}
	files := make([]string, 0, len(byFile))
	total := 0
	for file, list := range byFile {
		files = append(files, file)
		total += len(list)
	}
	sort.Strings(files)
	out := make([]T, 0, total)
	for _, file := range files {
		out = append(out, byFile[file]...)
	}
	return out
}

// allEdgesLocked returns every call edge, grouped by file in sorted order.
// The caller holds s.mu for reading or writing; the result is shared and
// must not be modified.
func (s *GOBSymbolStore) allEdgesLocked() []CallEdge {
	s.edgeCacheMu.Lock()
	defer s.edgeCacheMu.Unlock()
	if s.edgeCacheValid {
		return s.edgeCache
	}
	files := make([]string, 0, len(s.edges))
	total := 0
	for file, list := range s.edges {
		files = append(files, file)
		total += len(list)
	}
	sort.Strings(files)
	all := make([]CallEdge, 0, total)
	for _, file := range files {
		all = append(all, s.edges[file]...)
	}
	s.edgeCache = all
	s.edgeCacheValid = true
	return all
}

// SaveFile persists symbols and references for a file.
func (s *GOBSymbolStore) SaveFile(ctx context.Context, filePath string, symbols []Symbol, refs []Reference) error {
	return s.SaveFileWithContentHash(ctx, filePath, "", symbols, refs)
}

// SaveFileWithContentHash persists symbols/references for a file and tracks
// the current file content hash for future cache checks. The extractor
// version stays whatever was already stored (or empty) — call
// SaveFileWithSignature to update both fingerprints at once.
func (s *GOBSymbolStore) SaveFileWithContentHash(ctx context.Context, filePath string, contentHash string, symbols []Symbol, refs []Reference) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveFileUnlocked(filePath, contentHash, symbols, refs)
	return nil
}

// SaveFileWithSignature persists symbols/references for a file and
// records BOTH the content hash and the extractor version that produced
// them. Dedup callers can then ask GetFileExtractorVersion alongside
// GetFileContentHash and re-extract whenever either has drifted.
func (s *GOBSymbolStore) SaveFileWithSignature(ctx context.Context, filePath string, contentHash, extractorVersion string, symbols []Symbol, refs []Reference) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveFileUnlocked(filePath, contentHash, symbols, refs)
	current, ok := s.fileExtractorVersions[filePath]
	if extractorVersion != "" {
		if !ok || current != extractorVersion {
			s.fileExtractorVersions[filePath] = extractorVersion
			s.markDirtyUnlocked(filePath, false)
		}
	} else if ok {
		delete(s.fileExtractorVersions, filePath)
		s.markDirtyUnlocked(filePath, false)
	}
	return nil
}

// markDirtyUnlocked records that file changed; entries=true means its
// symbols/references/edges changed, not only its metadata.
func (s *GOBSymbolStore) markDirtyUnlocked(file string, entries bool) {
	s.mutationGeneration++
	s.lastSaved = s.now()
	s.dirty[file] = entries || s.dirty[file]
}

func (s *GOBSymbolStore) saveFileUnlocked(filePath, contentHash string, symbols []Symbol, refs []Reference) {
	if s.fileIndex[filePath] && s.sameEntriesUnlocked(filePath, symbols, refs) {
		// Re-extraction produced exactly what is stored: only the hash
		// can differ. Nothing else is touched, so nothing is persisted.
		current, ok := s.fileContentHashes[filePath]
		switch {
		case contentHash != "" && (!ok || current != contentHash):
			s.fileContentHashes[filePath] = contentHash
			s.markDirtyUnlocked(filePath, false)
		case contentHash == "" && ok:
			delete(s.fileContentHashes, filePath)
			s.markDirtyUnlocked(filePath, false)
		}
		return
	}

	// Remove old entries for this file first.
	s.removeEntriesUnlocked(filePath)

	for _, sym := range symbols {
		s.appendSymbolUnlocked(sym.Name, sym)
		if sym.File != filePath {
			s.dirty[sym.File] = true
		}
	}
	for _, ref := range refs {
		s.appendRefUnlocked(ref.SymbolName, ref)
		if ref.File != filePath {
			s.dirty[ref.File] = true
		}
	}
	for _, edge := range edgesFromRefs(refs) {
		s.edges[edge.File] = append(s.edges[edge.File], edge)
	}

	s.fileIndex[filePath] = true
	if contentHash != "" {
		s.fileContentHashes[filePath] = contentHash
	} else {
		delete(s.fileContentHashes, filePath)
	}
	s.invalidateEdgeCache()
	s.markDirtyUnlocked(filePath, true)
}

func edgesFromRefs(refs []Reference) []CallEdge {
	var edges []CallEdge
	for _, ref := range refs {
		if ref.CallerName != "" && ref.CallerName != "<top-level>" {
			edges = append(edges, CallEdge{
				Caller:   ref.CallerName,
				Callee:   ref.SymbolName,
				File:     ref.File,
				Line:     ref.Line,
				CallType: "direct",
			})
		}
	}
	return edges
}

// sameEntriesUnlocked reports whether saving symbols/refs for filePath would
// leave the store's entries exactly as they are.
func (s *GOBSymbolStore) sameEntriesUnlocked(filePath string, symbols []Symbol, refs []Reference) bool {
	symGroups := make(map[string]int, len(s.fileSymNames[filePath]))
	for _, sym := range symbols {
		if sym.File != filePath {
			return false
		}
		symGroups[sym.Name]++
	}
	if len(symGroups) != len(s.fileSymNames[filePath]) {
		return false
	}
	refGroups := make(map[string]int, len(s.fileRefNames[filePath]))
	for _, ref := range refs {
		if ref.File != filePath {
			return false
		}
		refGroups[ref.SymbolName]++
	}
	if len(refGroups) != len(s.fileRefNames[filePath]) {
		return false
	}
	// Walk the input in order; each name's stored group must match the
	// input's subsequence for that name element by element.
	seen := make(map[string]int, len(symGroups))
	for _, sym := range symbols {
		stored := s.symbols[sym.Name][filePath]
		i := seen[sym.Name]
		if len(stored) != symGroups[sym.Name] || stored[i] != sym {
			return false
		}
		seen[sym.Name] = i + 1
	}
	clear(seen)
	for _, ref := range refs {
		stored := s.refs[ref.SymbolName][filePath]
		i := seen[ref.SymbolName]
		if len(stored) != refGroups[ref.SymbolName] || stored[i] != ref {
			return false
		}
		seen[ref.SymbolName] = i + 1
	}
	want := edgesFromRefs(refs)
	have := s.edges[filePath]
	if len(want) != len(have) {
		return false
	}
	for i := range want {
		if want[i] != have[i] {
			return false
		}
	}
	return true
}

// DeleteFile removes all symbols and references for a file.
func (s *GOBSymbolStore) DeleteFile(ctx context.Context, filePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := s.removeEntriesUnlocked(filePath)
	if _, ok := s.fileExtractorVersions[filePath]; ok {
		delete(s.fileExtractorVersions, filePath)
		removed = true
	}
	if removed {
		s.markDirtyUnlocked(filePath, true)
	}
	return nil
}

// removeEntriesUnlocked drops every entry whose File is filePath plus the
// file's indexed flag and content hash. It touches only that file's groups.
func (s *GOBSymbolStore) removeEntriesUnlocked(filePath string) bool {
	removed := false
	for _, name := range s.fileSymNames[filePath] {
		byFile := s.symbols[name]
		n := len(byFile[filePath])
		s.removalVisits += uint64(n)
		s.totalSymbols -= n
		delete(byFile, filePath)
		if len(byFile) == 0 {
			delete(s.symbols, name)
		}
		removed = true
	}
	delete(s.fileSymNames, filePath)

	for _, name := range s.fileRefNames[filePath] {
		byFile := s.refs[name]
		n := len(byFile[filePath])
		s.removalVisits += uint64(n)
		s.totalRefs -= n
		delete(byFile, filePath)
		if len(byFile) == 0 {
			delete(s.refs, name)
		}
		removed = true
	}
	delete(s.fileRefNames, filePath)

	if edges, ok := s.edges[filePath]; ok {
		s.removalVisits += uint64(len(edges))
		delete(s.edges, filePath)
		s.invalidateEdgeCache()
		removed = true
	}

	if _, ok := s.fileIndex[filePath]; ok {
		delete(s.fileIndex, filePath)
		removed = true
	}
	if _, ok := s.fileContentHashes[filePath]; ok {
		delete(s.fileContentHashes, filePath)
		removed = true
	}
	return removed
}

// LookupSymbol finds symbol definitions by name, ordered by file path.
// Within a file, extraction order is preserved. Unlike the old flat store,
// cross-file insertion order is not retained; GetCallGraph picks the first
// file path when a name has multiple definitions.
func (s *GOBSymbolStore) LookupSymbol(ctx context.Context, name string) ([]Symbol, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byFile := s.symbols[name]
	if len(byFile) == 0 {
		return []Symbol{}, nil
	}
	return append([]Symbol(nil), flattenByFile(byFile)...), nil
}

// symbolsNamedLocked returns the shared (read-only) symbols for name.
func (s *GOBSymbolStore) symbolsNamedLocked(name string) []Symbol {
	byFile := s.symbols[name]
	if len(byFile) == 0 {
		return nil
	}
	return flattenByFile(byFile)
}

// LookupCallers finds all references/callers of a symbol.
func (s *GOBSymbolStore) LookupCallers(ctx context.Context, symbolName string) ([]Reference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byFile := s.refs[symbolName]
	if len(byFile) == 0 {
		return []Reference{}, nil
	}
	return filterByReferenceKinds(flattenByFile(byFile), RefKindCall, ""), nil
}

// LookupCallees finds all symbols called by a function.
func (s *GOBSymbolStore) LookupCallees(ctx context.Context, symbolName string, file string) ([]Reference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var callees []Reference
	seen := make(map[string]bool)

	for _, edge := range s.allEdgesLocked() {
		if edge.Caller == symbolName {
			key := fmt.Sprintf("%s:%d", edge.File, edge.Line)
			if seen[key] {
				continue
			}
			seen[key] = true

			// Find reference details
			for _, ref := range s.refs[edge.Callee][edge.File] {
				if !isCallReference(ref) {
					continue
				}
				if ref.CallerName == symbolName && ref.Line == edge.Line {
					callees = append(callees, ref)
					break
				}
			}

			// If no reference found, create a minimal one
			if !seen[key] || len(callees) == 0 {
				callees = append(callees, Reference{
					SymbolName: edge.Callee,
					File:       edge.File,
					Line:       edge.Line,
					CallerName: symbolName,
				})
			}
		}
	}
	return callees, nil
}

// LookupReaders finds property/data readers for a symbol name.
func (s *GOBSymbolStore) LookupReaders(ctx context.Context, symbolName string) ([]Reference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byFile := s.refs[symbolName]
	if len(byFile) == 0 {
		return []Reference{}, nil
	}
	return filterByReferenceKinds(flattenByFile(byFile), RefKindRead), nil
}

// LookupWriters finds property/data writers for a symbol name.
func (s *GOBSymbolStore) LookupWriters(ctx context.Context, symbolName string) ([]Reference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byFile := s.refs[symbolName]
	if len(byFile) == 0 {
		return []Reference{}, nil
	}
	return filterByReferenceKinds(flattenByFile(byFile), RefKindWrite), nil
}

func filterByReferenceKinds(refs []Reference, kinds ...string) []Reference {
	if len(refs) == 0 {
		return []Reference{}
	}

	allowed := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}

	filtered := make([]Reference, 0, len(refs))
	for _, ref := range refs {
		if allowed[ref.Kind] {
			filtered = append(filtered, ref)
			continue
		}
		// Backward compatibility with older indices where kind wasn't persisted.
		if ref.Kind == "" && allowed[RefKindCall] {
			filtered = append(filtered, ref)
		}
	}

	return filtered
}

func isCallReference(ref Reference) bool {
	return ref.Kind == "" || ref.Kind == RefKindCall
}

// GetCallGraph builds a call graph from a starting symbol.
func (s *GOBSymbolStore) GetCallGraph(ctx context.Context, symbolName string, depth int) (*CallGraph, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	graph := &CallGraph{
		Root:  symbolName,
		Nodes: make(map[string]Symbol),
		Edges: []CallEdge{},
		Depth: depth,
	}
	allEdges := s.allEdgesLocked()

	// BFS to build graph up to depth
	visited := make(map[string]bool)
	type queueItem struct {
		name  string
		depth int
	}
	queue := []queueItem{{symbolName, 0}}
	edgeSeen := make(map[string]bool)

	shouldTraverse := func(name string, isRoot bool) bool {
		symbols := s.symbolsNamedLocked(name)
		if len(symbols) == 0 {
			return false
		}
		// Root is explicitly requested by user and may be ambiguous.
		if isRoot {
			return true
		}
		// Avoid exploding through name-collided symbols (e.g. Load, Init).
		return len(symbols) == 1
	}
	isDeclarationSelfEdge := func(edge CallEdge) bool {
		if edge.Caller != edge.Callee {
			return false
		}
		for _, sym := range s.symbols[edge.Caller][edge.File] {
			if sym.Line == edge.Line {
				return true
			}
		}
		return false
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current.name] || current.depth > depth {
			continue
		}
		visited[current.name] = true

		// Add node
		if symbols := s.symbolsNamedLocked(current.name); len(symbols) > 0 {
			graph.Nodes[current.name] = symbols[0]
		}

		// Find edges (both callers and callees)
		for _, edge := range allEdges {
			if edge.Caller == current.name {
				if isDeclarationSelfEdge(edge) {
					continue
				}
				edgeKey := fmt.Sprintf("%s->%s", edge.Caller, edge.Callee)
				if !edgeSeen[edgeKey] {
					graph.Edges = append(graph.Edges, edge)
					edgeSeen[edgeKey] = true
				}
				if !visited[edge.Callee] && shouldTraverse(edge.Callee, false) {
					queue = append(queue, queueItem{edge.Callee, current.depth + 1})
				}
			}
			if current.depth == 0 && edge.Callee == current.name {
				if isDeclarationSelfEdge(edge) {
					continue
				}
				edgeKey := fmt.Sprintf("%s->%s", edge.Caller, edge.Callee)
				if !edgeSeen[edgeKey] {
					graph.Edges = append(graph.Edges, edge)
					edgeSeen[edgeKey] = true
				}
				// Ensure caller node is present in the graph.
				if _, exists := graph.Nodes[edge.Caller]; !exists {
					if syms := s.symbolsNamedLocked(edge.Caller); len(syms) > 0 {
						graph.Nodes[edge.Caller] = syms[0]
					}
				}
			}
		}
	}

	return graph, nil
}

// GetSymbolsForFile returns all symbols defined in a specific file.
func (s *GOBSymbolStore) GetSymbolsForFile(ctx context.Context, filePath string) ([]Symbol, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []Symbol
	for _, name := range s.fileSymNames[filePath] {
		result = append(result, s.symbols[name][filePath]...)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Line != result[j].Line {
			return result[i].Line < result[j].Line
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

// GetCallEdges returns all call graph edges.
func (s *GOBSymbolStore) GetCallEdges(ctx context.Context) ([]CallEdge, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	all := s.allEdgesLocked()
	edges := make([]CallEdge, len(all))
	copy(edges, all)
	return edges, nil
}

// Close shuts down the store.
func (s *GOBSymbolStore) Close() error {
	return s.Persist(context.Background())
}

// GetStats returns statistics about the symbol index.
func (s *GOBSymbolStore) GetStats(ctx context.Context) (*SymbolStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var size int64
	baseMatches := false
	if info, err := os.Stat(s.indexPath); err == nil {
		size = info.Size()
		baseMatches = s.baseStamp.valid && size == s.baseStamp.size && info.ModTime().Equal(s.baseStamp.modTime)
	}
	if info, err := os.Stat(s.logPath); err == nil && baseMatches && s.logID != "" && s.logHeaderMatchesUnlocked() {
		size += info.Size()
	}

	return &SymbolStats{
		TotalSymbols:    s.totalSymbols,
		TotalReferences: s.totalRefs,
		TotalFiles:      len(s.fileIndex),
		IndexSize:       size,
		LastUpdated:     s.updatedAt,
	}, nil
}

// IsFileIndexed checks if a file has been indexed.
func (s *GOBSymbolStore) IsFileIndexed(filePath string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fileIndex[filePath]
}

// GetFileContentHash returns the stored content hash for a file when available.
func (s *GOBSymbolStore) GetFileContentHash(filePath string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hash, ok := s.fileContentHashes[filePath]
	return hash, ok
}

// GetFileExtractorVersion returns the SymbolExtractor.Version() that was
// recorded when this file's symbols were last persisted, if known.
// Symbol stores produced by older grepai releases lack this metadata; in
// that case the second return is false and callers should treat the
// cache as stale.
func (s *GOBSymbolStore) GetFileExtractorVersion(filePath string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	version, ok := s.fileExtractorVersions[filePath]
	return version, ok
}
