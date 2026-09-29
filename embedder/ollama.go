package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	defaultOllamaEndpoint = "http://localhost:11434"
	defaultOllamaModel    = "nomic-embed-text"
	nomicEmbedDimensions  = 768

	// defaultOllamaParallelism is the number of /api/embed requests kept in
	// flight at once by EmbedBatches when no parallelism is configured.
	defaultOllamaParallelism = 4

	// ollamaMaxBatchInputs bounds the number of inputs sent in one /api/embed
	// request. Ollama processes the inputs of a request sequentially on the
	// loaded model, so a request of this size keeps the per-request HTTP and
	// JSON overhead small without producing multi-second requests.
	ollamaMaxBatchInputs = 64

	// ollamaMaxBatchChars bounds the total input text of one /api/embed
	// request so a batch of large chunks does not become a huge request body.
	ollamaMaxBatchChars = 256 * 1024
)

type OllamaEmbedder struct {
	endpoint    string
	model       string
	dimensions  int
	parallelism int
	client      *http.Client
}

// ollamaEmbedRequest is the body of POST /api/embed (the batched endpoint).
// Truncate is sent as false so an over-long input fails with a context-length
// error instead of being silently cut, which lets the indexer re-chunk it.
type ollamaEmbedRequest struct {
	Model    string   `json:"model"`
	Input    []string `json:"input"`
	Truncate bool     `json:"truncate"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type OllamaOption func(*OllamaEmbedder)

func WithOllamaEndpoint(endpoint string) OllamaOption {
	return func(e *OllamaEmbedder) {
		e.endpoint = endpoint
	}
}

func WithOllamaModel(model string) OllamaOption {
	return func(e *OllamaEmbedder) {
		e.model = model
	}
}
func WithOllamaDimensions(dimensions int) OllamaOption {
	return func(e *OllamaEmbedder) {
		e.dimensions = dimensions
	}
}

// WithOllamaParallelism sets how many /api/embed requests EmbedBatches keeps
// in flight at once. Values <= 0 are ignored, preserving the default.
func WithOllamaParallelism(parallelism int) OllamaOption {
	return func(e *OllamaEmbedder) {
		if parallelism > 0 {
			e.parallelism = parallelism
		}
	}
}

// WithOllamaTimeout overrides the HTTP client timeout for embedding requests.
// Values <= 0 are ignored, preserving the default.
func WithOllamaTimeout(d time.Duration) OllamaOption {
	return func(e *OllamaEmbedder) {
		if d > 0 && e.client != nil {
			e.client.Timeout = d
		}
	}
}

func NewOllamaEmbedder(opts ...OllamaOption) *OllamaEmbedder {
	e := &OllamaEmbedder{
		endpoint:    defaultOllamaEndpoint,
		model:       defaultOllamaModel,
		dimensions:  nomicEmbedDimensions,
		parallelism: defaultOllamaParallelism,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// Embed converts one text into a vector with a single /api/embed request.
func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.embedInputs(ctx, []string{text}, false)
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// EmbedBatch embeds texts in order, sending up to ollamaMaxBatchInputs inputs
// (or ollamaMaxBatchChars characters) per /api/embed request. When Ollama
// rejects a request because an input exceeds the model's context length, the
// inputs of that request are embedded one at a time to find the offending
// input, and a ContextLengthError carrying its index in texts is returned so
// the indexer can re-chunk it.
func (e *OllamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))

	for start := 0; start < len(texts); {
		end := e.subBatchEnd(texts, start)
		vectors, err := e.embedInputs(ctx, texts[start:end], false)
		if err != nil {
			if AsContextLengthError(err) == nil {
				return nil, fmt.Errorf("failed to embed texts %d-%d: %w", start, end-1, err)
			}
			offending, locErr := e.locateContextLengthError(ctx, texts[start:end])
			if locErr != nil {
				return nil, locErr
			}
			ctxErr := AsContextLengthError(err)
			ctxErr.ChunkIndex = start + offending
			ctxErr.EstimatedTokens = EstimateTokens(texts[start+offending])
			return nil, ctxErr
		}
		copy(results[start:end], vectors)
		start = end
	}

	return results, nil
}

// EmbedBatches implements BatchEmbedder. Each Batch is embedded with the same
// sub-batching as EmbedBatch, with up to parallelism requests in flight. An
// input that exceeds the context length is embedded again with truncation so
// one oversized chunk cannot fail a whole index run; the indexer's batched
// path has no per-file re-chunking to fall back to.
func (e *OllamaEmbedder) EmbedBatches(ctx context.Context, batches []Batch, progress BatchProgress) ([]BatchResult, error) {
	if len(batches) == 0 {
		return nil, nil
	}

	totalChunks := 0
	for _, batch := range batches {
		totalChunks += batch.Size()
	}

	var completedChunks atomic.Int64
	results := make([]BatchResult, len(batches))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(e.parallelism)

	for i := range batches {
		batch := batches[i]
		g.Go(func() error {
			embeddings, err := e.embedBatchTruncatingOversized(ctx, batch)
			if err != nil {
				return fmt.Errorf("batch %d/%d: %w", batch.Index+1, len(batches), err)
			}
			if batch.Index < 0 || batch.Index >= len(results) {
				return fmt.Errorf("batch index %d out of range (%d batches)", batch.Index, len(batches))
			}
			results[batch.Index] = BatchResult{
				BatchIndex: batch.Index,
				Embeddings: embeddings,
			}
			completed := completedChunks.Add(int64(batch.Size()))
			if progress != nil {
				progress(batch.Index, len(batches), int(completed), totalChunks, false, 0, http.StatusOK)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return results, nil
}

// embedBatchTruncatingOversized embeds one Batch's contents. When an input is
// too long for the model it is embedded on its own with truncate=true and the
// event is logged with the source file/chunk so it can be found.
func (e *OllamaEmbedder) embedBatchTruncatingOversized(ctx context.Context, batch Batch) ([][]float32, error) {
	texts := batch.Contents()
	results := make([][]float32, len(texts))

	for start := 0; start < len(texts); {
		end := e.subBatchEnd(texts, start)
		vectors, err := e.embedInputs(ctx, texts[start:end], false)
		if err == nil {
			copy(results[start:end], vectors)
			start = end
			continue
		}
		if AsContextLengthError(err) == nil {
			return nil, err
		}

		// Embed this sub-batch one input at a time, truncating the oversized ones.
		for i := start; i < end; i++ {
			vec, err := e.embedInputs(ctx, texts[i:i+1], false)
			if err != nil && AsContextLengthError(err) != nil {
				entry := batch.Entries[i]
				log.Printf("Ollama: input exceeds context length (file %d chunk %d, ~%d tokens); embedding truncated",
					entry.FileIndex, entry.ChunkIndex, EstimateTokens(texts[i]))
				vec, err = e.embedInputs(ctx, texts[i:i+1], true)
			}
			if err != nil {
				return nil, err
			}
			results[i] = vec[0]
		}
		start = end
	}

	return results, nil
}

// subBatchEnd returns the exclusive end index of the sub-batch starting at
// start, bounded by ollamaMaxBatchInputs inputs and ollamaMaxBatchChars
// characters. It always advances by at least one input.
func (e *OllamaEmbedder) subBatchEnd(texts []string, start int) int {
	end := start
	chars := 0
	for end < len(texts) && end-start < ollamaMaxBatchInputs {
		chars += len(texts[end])
		if end > start && chars > ollamaMaxBatchChars {
			break
		}
		end++
	}
	return end
}

// locateContextLengthError embeds texts one at a time and returns the index of
// the first one Ollama rejects for exceeding the context length.
func (e *OllamaEmbedder) locateContextLengthError(ctx context.Context, texts []string) (int, error) {
	for i, text := range texts {
		_, err := e.embedInputs(ctx, []string{text}, false)
		if err == nil {
			continue
		}
		if AsContextLengthError(err) != nil {
			return i, nil
		}
		return 0, fmt.Errorf("failed to embed text %d: %w", i, err)
	}
	return 0, fmt.Errorf("Ollama rejected a batch of %d inputs for context length, but every input embeds on its own", len(texts))
}

// embedInputs sends one POST /api/embed request for inputs and returns one
// vector per input, in order.
func (e *OllamaEmbedder) embedInputs(ctx context.Context, inputs []string, truncate bool) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	reqBody := ollamaEmbedRequest{
		Model:    e.model,
		Input:    inputs,
		Truncate: truncate,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/embed", e.endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)

		// Ollama answers 400 (or 500 on older builds) with this message when an
		// input is longer than the model's context window and truncate is false.
		if strings.Contains(bodyStr, "exceeds the context length") {
			totalChars := 0
			for _, in := range inputs {
				totalChars += len(in)
			}
			return nil, NewContextLengthError(0, (totalChars+3)/4, 0, bodyStr)
		}

		return nil, fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, bodyStr)
	}

	var result ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(result.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("Ollama returned %d embeddings for %d inputs", len(result.Embeddings), len(inputs))
	}
	for i, vec := range result.Embeddings {
		if len(vec) == 0 {
			return nil, fmt.Errorf("Ollama returned empty embedding for input %d", i)
		}
	}

	return result.Embeddings, nil
}

func (e *OllamaEmbedder) Dimensions() int {
	return e.dimensions
}

func (e *OllamaEmbedder) Close() error {
	return nil
}

// Ping checks if Ollama is reachable
func (e *OllamaEmbedder) Ping(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/tags", e.endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to reach Ollama at %s: %w", e.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Ollama returned status %d", resp.StatusCode)
	}

	return nil
}
