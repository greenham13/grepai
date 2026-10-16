package mcp

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/trace"
)

// Keep one large project, or up to eight small ones. A workspace status
// request must not retain every project's decoded index indefinitely.
const symbolCacheBytes int64 = 512 << 20

type symbolCache struct {
	mu      sync.Mutex
	entries map[string]symbolCacheEntry
	next    uint64
}
type symbolCacheEntry struct {
	store *trace.GOBSymbolStore
	stamp symbolDiskStamp
	used  uint64
}
type symbolDiskStamp struct{ base, log os.FileInfo }

func symbolStamp(path string) (symbolDiskStamp, error) {
	var stamp symbolDiskStamp
	for _, item := range []struct {
		path string
		dst  *os.FileInfo
	}{
		{path, &stamp.base}, {path + ".log", &stamp.log},
	} {
		info, err := os.Stat(item.path)
		if err != nil && !os.IsNotExist(err) {
			return stamp, err
		}
		*item.dst = info
	}
	return stamp, nil
}
func sameSymbolFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
func (a symbolDiskStamp) equal(b symbolDiskStamp) bool {
	return sameSymbolFile(a.base, b.base) && sameSymbolFile(a.log, b.log)
}
func (a symbolDiskStamp) size() int64 {
	var n int64
	if a.base != nil {
		n += a.base.Size()
	}
	if a.log != nil {
		n += a.log.Size()
	}
	return n
}
func (s *Server) loadSymbolStore(ctx context.Context, root string) (*trace.GOBSymbolStore, error) {
	return s.symbols.load(ctx, config.GetSymbolIndexPath(root))
}
func (c *symbolCache) load(ctx context.Context, path string) (*trace.GOBSymbolStore, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := symbolStamp(path)
	if err != nil {
		return nil, err
	}
	c.next++
	if entry, ok := c.entries[path]; ok && entry.stamp.equal(before) {
		entry.used = c.next
		c.entries[path] = entry
		return entry.store, nil
	}
	delete(c.entries, path)
	st := trace.NewGOBSymbolStore(path)
	if err := st.Load(ctx); err != nil {
		return nil, err
	}
	after, err := symbolStamp(path)
	if err != nil {
		return nil, err
	}
	// A writer raced the load. The loaded snapshot is valid, but its stamp
	// is not known; let the next request load it again.
	if !before.equal(after) {
		return st, nil
	}
	if c.entries == nil {
		c.entries = make(map[string]symbolCacheEntry)
	}
	c.entries[path] = symbolCacheEntry{store: st, stamp: after, used: c.next}
	for len(c.entries) > 1 {
		var total int64
		oldest, oldestUse := "", ^uint64(0)
		for key, entry := range c.entries {
			total += entry.stamp.size()
			if key != path && entry.used < oldestUse {
				oldest, oldestUse = key, entry.used
			}
		}
		if total <= symbolCacheBytes && len(c.entries) <= 8 {
			break
		}
		delete(c.entries, oldest)
	}
	return st, nil
}

func (s *Server) loadWorkspaceSymbolStores(ctx context.Context, workspace, project string) ([]trace.SymbolStore, error) {
	cfg, err := config.LoadWorkspaceConfig()
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("no workspaces configured")
	}
	ws, err := cfg.GetWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	stores := make([]trace.SymbolStore, 0, len(ws.Projects))
	for _, p := range ws.Projects {
		if project != "" && project != p.Name {
			continue
		}
		st, err := s.loadSymbolStore(ctx, p.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to load symbol index for project %s: %w", p.Name, err)
		}
		stores = append(stores, st)
	}
	if project != "" && len(stores) == 0 {
		return nil, fmt.Errorf("project %q not found in workspace %q", project, workspace)
	}
	return stores, nil
}
