package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Reconcile walks outside the event writer, then submits one file at a time.
// apply must serialize each operation with live file events. Fresh stats and
// reads inside apply prevent an older walk result from overwriting a live edit.
// onFile updates dependent indexes inside that same operation (nil means delete).
func (idx *Indexer) Reconcile(ctx context.Context, apply func(func() error) error, onFile func(string, *FileMeta, bool) error) (*IndexStats, error) {
	return idx.ReconcileWithDocuments(ctx, idx.store.ListDocuments, apply, onFile)
}

// ReconcileWithDocuments is Reconcile with the store listing supplied by the
// caller. A workspace startup queue walks hundreds of projects against one
// shared collection; listing that collection per project (millions of points
// each time) is what the shared list avoids. list must return paths relative
// to this root, as the store's own ListDocuments would.
func (idx *Indexer) ReconcileWithDocuments(ctx context.Context, list func(context.Context) ([]string, error), apply func(func() error) error, onFile func(string, *FileMeta, bool) error) (*IndexStats, error) {
	start := time.Now()
	stats := &IndexStats{}
	if list == nil {
		list = idx.store.ListDocuments
	}
	docs, err := list(ctx)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(docs))
	for _, path := range docs {
		present[path] = true
	}
	// Serialize invalidation with live snapshot updates.
	if err := apply(func() error { idx.snapshot.reconcile(present); return nil }); err != nil {
		return nil, err
	}
	metas, skipped, err := idx.scanMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to scan files: %w", err)
	}
	stats.FilesSkipped = len(skipped)
	for _, meta := range metas {
		delete(present, meta.Path)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := apply(func() error {
			info, err := os.Stat(filepath.Join(idx.root, meta.Path))
			if os.IsNotExist(err) {
				return nil
			} // The live delete owns this file.
			if err != nil {
				return err
			}
			meta.Size, meta.ModTime, meta.ModTimeNano = info.Size(), info.ModTime().Unix(), info.ModTime().UnixNano()
			decision, err := idx.decideFileScanChecked(ctx, meta, true)
			if err != nil {
				return err
			}
			if decision.file != nil {
				chunks, err := idx.IndexFile(ctx, *decision.file)
				if err != nil {
					return err
				}
				stats.FilesIndexed++
				stats.ChunksCreated += chunks
			} else {
				stats.FilesSkipped++
			}
			if onFile != nil {
				return onFile(meta.Path, &meta, decision.file != nil)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for path := range present {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := apply(func() error {
			// A file created after enumeration must not be removed by this old walk.
			if _, err := os.Stat(filepath.Join(idx.root, path)); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := idx.RemoveFile(ctx, path); err != nil {
				return err
			}
			stats.FilesRemoved++
			if onFile != nil {
				return onFile(path, nil, true)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	stats.Duration = time.Since(start)
	return stats, nil
}
