package cli

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/yoanbernabeu/grepai/store"
)

// Mutations arm one timer. Taking a batch detaches it before any IO, so a
// change during persistence remains queued for the next batch.
type workspacePersistence struct {
	mu         sync.Mutex
	dirty      map[string]*workspaceProjectRuntime
	ticks      chan time.Time
	timer      *time.Timer
	generation uint64
	delay      time.Duration
	closed     bool
}

func newWorkspacePersistence(delay time.Duration) *workspacePersistence {
	return &workspacePersistence{dirty: make(map[string]*workspaceProjectRuntime), ticks: make(chan time.Time, 1), delay: delay}
}

// track runs before the project's writer and RPG workers start.
func (p *workspacePersistence) track(r *workspaceProjectRuntime) {
	mark := func() { p.mark(r) }
	r.symbolStore.SetMutationHook(mark)
	r.idx.SetMutationHook(mark)
	if r.manager != nil {
		r.manager.onMutation = mark
	}
	if st, ok := r.vectorStore.(*projectPrefixStore); ok {
		st.onMutation = mark
	}
}

func (p *workspacePersistence) mark(r *workspaceProjectRuntime) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.dirty[r.project.Path] = r
	if p.timer != nil {
		return
	}
	p.generation++
	generation := p.generation
	p.timer = time.AfterFunc(p.delay, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed || p.generation != generation {
			return
		}
		select {
		case p.ticks <- time.Now():
		default:
		}
	})
}

func (p *workspacePersistence) take() map[string]*workspaceProjectRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.generation++
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	select {
	case <-p.ticks:
	default:
	}
	batch := p.dirty
	p.dirty = make(map[string]*workspaceProjectRuntime)
	return batch
}

func (p *workspacePersistence) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.generation++
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

func (p *workspacePersistence) persist(ctx context.Context, fence *watchMutationFence, st store.VectorStore) error {
	return fence.handle(ctx, func(ctx context.Context) {
		batch := p.take()
		if len(batch) == 0 {
			return
		}
		if err := persistWorkspaceStores(ctx, st, batch); err != nil {
			log.Printf("Warning: failed to persist changed workspace indexes: %v", err)
			for _, r := range batch {
				p.mark(r)
			}
		}
	})
}
