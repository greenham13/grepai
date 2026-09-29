package embedder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOllama serves POST /api/embed. Inputs containing "TOOLONG" are rejected
// with Ollama's context-length error unless truncate is true.
type fakeOllama struct {
	t          *testing.T
	mu         sync.Mutex
	requests   []ollamaEmbedRequest
	legacyHits int
	inFlight   atomic.Int32
	maxFlight  atomic.Int32
	delay      time.Duration
	failStatus int // when non-zero, every request fails with this status
}

func (f *fakeOllama) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/embeddings" {
		f.mu.Lock()
		f.legacyHits++
		f.mu.Unlock()
		http.Error(w, "legacy endpoint must not be used", http.StatusTeapot)
		return
	}
	if r.URL.Path != "/api/embed" || r.Method != http.MethodPost {
		http.Error(w, "unexpected route "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		return
	}

	cur := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		prev := f.maxFlight.Load()
		if cur <= prev || f.maxFlight.CompareAndSwap(prev, cur) {
			break
		}
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}

	var req ollamaEmbedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if f.failStatus != 0 {
		http.Error(w, `{"error":"boom"}`, f.failStatus)
		return
	}

	out := ollamaEmbedResponse{Embeddings: make([][]float32, 0, len(req.Input))}
	for _, in := range req.Input {
		if strings.Contains(in, "TOOLONG") && !req.Truncate {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"the input length exceeds the context length"}`))
			return
		}
		out.Embeddings = append(out.Embeddings, vectorFor(in))
	}
	_ = json.NewEncoder(w).Encode(out)
}

// vectorFor gives each input a recognisable vector: [len, first byte].
func vectorFor(in string) []float32 {
	var first float32
	if len(in) > 0 {
		first = float32(in[0])
	}
	return []float32{float32(len(in)), first}
}

func newFakeOllama(t *testing.T) (*fakeOllama, *OllamaEmbedder) {
	f := &fakeOllama{t: t}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return f, NewOllamaEmbedder(WithOllamaEndpoint(srv.URL), WithOllamaModel("test-model"))
}

func (f *fakeOllama) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestOllamaEmbed_UsesBatchEndpoint(t *testing.T) {
	f, e := newFakeOllama(t)

	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if want := vectorFor("hello"); vec[0] != want[0] || vec[1] != want[1] {
		t.Fatalf("vector = %v, want %v", vec, want)
	}
	if f.legacyHits != 0 {
		t.Fatalf("legacy /api/embeddings was called %d times", f.legacyHits)
	}
	if got := f.requestCount(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	req := f.requests[0]
	if req.Model != "test-model" || len(req.Input) != 1 || req.Input[0] != "hello" || req.Truncate {
		t.Fatalf("unexpected request %+v", req)
	}
}

func TestOllamaEmbedBatch_OneRequestPerSubBatch(t *testing.T) {
	f, e := newFakeOllama(t)

	texts := make([]string, 150)
	for i := range texts {
		texts[i] = fmt.Sprintf("chunk-%03d", i)
	}
	vecs, err := e.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("got %d vectors, want %d", len(vecs), len(texts))
	}
	for i, v := range vecs {
		if want := vectorFor(texts[i]); v[0] != want[0] || v[1] != want[1] {
			t.Fatalf("vector %d = %v, want %v", i, v, want)
		}
	}
	// 150 inputs at 64 per request → 64 + 64 + 22.
	if got := f.requestCount(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	if n := len(f.requests[0].Input); n != ollamaMaxBatchInputs {
		t.Fatalf("first request had %d inputs, want %d", n, ollamaMaxBatchInputs)
	}
	if n := len(f.requests[2].Input); n != 22 {
		t.Fatalf("last request had %d inputs, want 22", n)
	}
	for _, r := range f.requests {
		if r.Truncate {
			t.Fatal("truncate must be false so over-long inputs fail loudly")
		}
	}
}

func TestOllamaEmbedBatch_SplitsOnCharacterBudget(t *testing.T) {
	f, e := newFakeOllama(t)

	big := strings.Repeat("x", ollamaMaxBatchChars/2+1)
	texts := []string{big, big, big}
	if _, err := e.EmbedBatch(context.Background(), texts); err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	// Two of these exceed the character budget, so each goes alone.
	if got := f.requestCount(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestOllamaEmbedBatch_LocatesOverLongChunk(t *testing.T) {
	f, e := newFakeOllama(t)

	texts := []string{"a", "b", "TOOLONG here", "d"}
	_, err := e.EmbedBatch(context.Background(), texts)
	if err == nil {
		t.Fatal("expected a context length error")
	}
	ctxErr := AsContextLengthError(err)
	if ctxErr == nil {
		t.Fatalf("expected ContextLengthError, got %T: %v", err, err)
	}
	if ctxErr.ChunkIndex != 2 {
		t.Fatalf("ChunkIndex = %d, want 2", ctxErr.ChunkIndex)
	}
	// 1 batch request + 3 single probes (a, b, TOOLONG).
	if got := f.requestCount(); got != 4 {
		t.Fatalf("requests = %d, want 4", got)
	}
}

func TestOllamaEmbedBatch_ChunkIndexIsAbsoluteAcrossSubBatches(t *testing.T) {
	_, e := newFakeOllama(t)

	texts := make([]string, 70)
	for i := range texts {
		texts[i] = fmt.Sprintf("chunk-%03d", i)
	}
	texts[67] = "TOOLONG"
	_, err := e.EmbedBatch(context.Background(), texts)
	ctxErr := AsContextLengthError(err)
	if ctxErr == nil {
		t.Fatalf("expected ContextLengthError, got %v", err)
	}
	if ctxErr.ChunkIndex != 67 {
		t.Fatalf("ChunkIndex = %d, want 67", ctxErr.ChunkIndex)
	}
}

func TestOllamaEmbedBatch_ServerErrorIsNotContextLength(t *testing.T) {
	f, e := newFakeOllama(t)
	f.failStatus = http.StatusInternalServerError

	_, err := e.EmbedBatch(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error")
	}
	if AsContextLengthError(err) != nil {
		t.Fatalf("a generic 500 must not be reported as a context length error: %v", err)
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("error should carry the status: %v", err)
	}
	if got := f.requestCount(); got != 1 {
		t.Fatalf("requests = %d, want 1 (no per-input probing on generic errors)", got)
	}
}

func TestOllamaEmbedInputs_RejectsCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float32{{1}}})
	}))
	t.Cleanup(srv.Close)
	e := NewOllamaEmbedder(WithOllamaEndpoint(srv.URL))

	_, err := e.EmbedBatch(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "returned 1 embeddings for 2 inputs") {
		t.Fatalf("expected count mismatch error, got %v", err)
	}
}

func TestOllamaEmbedInputs_RejectsEmptyVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float32{{1}, {}}})
	}))
	t.Cleanup(srv.Close)
	e := NewOllamaEmbedder(WithOllamaEndpoint(srv.URL))

	_, err := e.EmbedBatch(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "empty embedding for input 1") {
		t.Fatalf("expected empty embedding error, got %v", err)
	}
}

func TestOllamaEmbedBatches_ImplementsBatchEmbedder(t *testing.T) {
	var _ BatchEmbedder = (*OllamaEmbedder)(nil)
}

func TestOllamaEmbedBatches_OrderedResultsAndBoundedParallelism(t *testing.T) {
	f, e := newFakeOllama(t)
	f.delay = 20 * time.Millisecond
	WithOllamaParallelism(3)(e)

	files := make([]FileChunks, 10)
	for i := range files {
		files[i] = FileChunks{FileIndex: i, Chunks: []string{fmt.Sprintf("f%d-c0", i), fmt.Sprintf("f%d-c1", i)}}
	}
	// One batch per file so parallelism is exercised.
	batches := make([]Batch, len(files))
	for i, fc := range files {
		for j, c := range fc.Chunks {
			batches[i].Entries = append(batches[i].Entries, BatchEntry{FileIndex: fc.FileIndex, ChunkIndex: j, Content: c})
		}
		batches[i].Index = i
	}

	var progressCalls atomic.Int32
	var lastCompleted atomic.Int32
	results, err := e.EmbedBatches(context.Background(), batches, func(batchIndex, totalBatches, completedChunks, totalChunks int, retrying bool, attempt int, statusCode int) {
		progressCalls.Add(1)
		if totalBatches != 10 || totalChunks != 20 {
			t.Errorf("progress totals = %d batches / %d chunks, want 10 / 20", totalBatches, totalChunks)
		}
		for {
			prev := lastCompleted.Load()
			if int32(completedChunks) <= prev || lastCompleted.CompareAndSwap(prev, int32(completedChunks)) {
				break
			}
		}
	})
	if err != nil {
		t.Fatalf("EmbedBatches: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("got %d results, want 10", len(results))
	}
	for i, r := range results {
		if r.BatchIndex != i {
			t.Fatalf("result %d has BatchIndex %d", i, r.BatchIndex)
		}
		for j, v := range r.Embeddings {
			if want := vectorFor(batches[i].Entries[j].Content); v[0] != want[0] || v[1] != want[1] {
				t.Fatalf("batch %d entry %d vector %v, want %v", i, j, v, want)
			}
		}
	}
	if got := f.maxFlight.Load(); got > 3 {
		t.Fatalf("max in-flight requests = %d, want <= 3", got)
	}
	if got := f.maxFlight.Load(); got < 2 {
		t.Fatalf("max in-flight requests = %d, expected parallel requests", got)
	}
	if progressCalls.Load() != 10 {
		t.Fatalf("progress calls = %d, want 10", progressCalls.Load())
	}
	if lastCompleted.Load() != 20 {
		t.Fatalf("final completedChunks = %d, want 20", lastCompleted.Load())
	}

	fileEmbeddings := MapResultsToFiles(batches, results, len(files))
	for i, fc := range files {
		if len(fileEmbeddings[i]) != len(fc.Chunks) {
			t.Fatalf("file %d mapped %d embeddings, want %d", i, len(fileEmbeddings[i]), len(fc.Chunks))
		}
	}
}

func TestOllamaEmbedBatches_TruncatesOversizedInputInsteadOfFailing(t *testing.T) {
	f, e := newFakeOllama(t)

	batch := Batch{Index: 0, Entries: []BatchEntry{
		{FileIndex: 0, ChunkIndex: 0, Content: "ok-1"},
		{FileIndex: 3, ChunkIndex: 2, Content: "TOOLONG chunk"},
		{FileIndex: 0, ChunkIndex: 1, Content: "ok-2"},
	}}
	results, err := e.EmbedBatches(context.Background(), []Batch{batch}, nil)
	if err != nil {
		t.Fatalf("EmbedBatches: %v", err)
	}
	if len(results) != 1 || len(results[0].Embeddings) != 3 {
		t.Fatalf("unexpected results %+v", results)
	}
	for i, entry := range batch.Entries {
		if want := vectorFor(entry.Content); results[0].Embeddings[i][0] != want[0] {
			t.Fatalf("entry %d vector %v, want %v", i, results[0].Embeddings[i], want)
		}
	}
	var truncated int
	for _, r := range f.requests {
		if r.Truncate {
			truncated++
			if len(r.Input) != 1 || r.Input[0] != "TOOLONG chunk" {
				t.Fatalf("truncate request for unexpected input %v", r.Input)
			}
		}
	}
	if truncated != 1 {
		t.Fatalf("truncate requests = %d, want 1", truncated)
	}
}

func TestOllamaEmbedBatches_PropagatesServerError(t *testing.T) {
	f, e := newFakeOllama(t)
	f.failStatus = http.StatusBadGateway

	_, err := e.EmbedBatches(context.Background(), []Batch{{Index: 0, Entries: []BatchEntry{{Content: "a"}}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("expected 502 error, got %v", err)
	}
}

func TestOllamaEmbedBatches_HonoursContextCancel(t *testing.T) {
	f, e := newFakeOllama(t)
	f.delay = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.EmbedBatches(ctx, []Batch{{Index: 0, Entries: []BatchEntry{{Content: "a"}}}}, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestOllamaSubBatchEnd_AlwaysAdvances(t *testing.T) {
	e := NewOllamaEmbedder()
	texts := []string{strings.Repeat("x", ollamaMaxBatchChars*2), "small"}
	if end := e.subBatchEnd(texts, 0); end != 1 {
		t.Fatalf("subBatchEnd = %d, want 1 (a single over-budget input still forms a batch)", end)
	}
	if end := e.subBatchEnd(texts, 1); end != 2 {
		t.Fatalf("subBatchEnd from 1 = %d, want 2", end)
	}
}

func TestWithOllamaParallelism_IgnoresNonPositive(t *testing.T) {
	e := NewOllamaEmbedder(WithOllamaParallelism(0), WithOllamaParallelism(-3))
	if e.parallelism != defaultOllamaParallelism {
		t.Fatalf("parallelism = %d, want default %d", e.parallelism, defaultOllamaParallelism)
	}
	WithOllamaParallelism(7)(e)
	if e.parallelism != 7 {
		t.Fatalf("parallelism = %d, want 7", e.parallelism)
	}
}
