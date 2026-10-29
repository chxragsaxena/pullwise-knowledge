package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Default Cohere models; they share an embedding space.
const (
	DefaultCohereDocumentModel = "embed-v5.0-pro"
	DefaultCohereQueryModel    = "embed-v5.0-fast"
)

// maxTextsPerCall is Cohere's documented limit for one Embed request.
const maxTextsPerCall = 96

const cohereDefaultBaseURL = "https://api.cohere.com"

// CohereEmbedder implements Embedder over Cohere's v2 Embed API.
type CohereEmbedder struct {
	apiKey        string
	documentModel string
	queryModel    string
	baseURL       string
	http          *http.Client
}

// CohereOption configures a CohereEmbedder.
type CohereOption func(*CohereEmbedder)

// WithCohereBaseURL points the client at another host (used by tests).
func WithCohereBaseURL(baseURL string) CohereOption {
	return func(e *CohereEmbedder) { e.baseURL = baseURL }
}

// NewCohereEmbedder builds an embedder authenticated with apiKey.
func NewCohereEmbedder(apiKey string, opts ...CohereOption) *CohereEmbedder {
	e := &CohereEmbedder{
		apiKey:        apiKey,
		documentModel: DefaultCohereDocumentModel,
		queryModel:    DefaultCohereQueryModel,
		baseURL:       cohereDefaultBaseURL,
		http:          &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

type cohereRequest struct {
	Model           string   `json:"model"`
	InputType       string   `json:"input_type"`
	Texts           []string `json:"texts"`
	EmbeddingTypes  []string `json:"embedding_types"`
	OutputDimension int      `json:"output_dimension"`
}

type cohereResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

type cohereErrorBody struct {
	Message string `json:"message"`
}

// CohereAPIError is a non-2xx response from Cohere.
type CohereAPIError struct {
	StatusCode int
	Message    string
}

func (e *CohereAPIError) Error() string {
	return fmt.Sprintf("cohere: %d: %s", e.StatusCode, e.Message)
}

// Embed returns one vector per text in as few API calls as possible.
func (e *CohereEmbedder) Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error) {
	model, inputType := e.documentModel, "search_document"
	if kind == Query {
		model, inputType = e.queryModel, "search_query"
	}

	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxTextsPerCall {
		batch := texts[start:min(start+maxTextsPerCall, len(texts))]
		got, err := e.call(ctx, cohereRequest{
			Model: model, InputType: inputType, Texts: batch,
			EmbeddingTypes: []string{"float"}, OutputDimension: Dimension,
		})
		if err != nil {
			return nil, fmt.Errorf("cohere embed (texts %d-%d of %d): %w", start+1, start+len(batch), len(texts), err)
		}
		if len(got) != len(batch) {
			return nil, fmt.Errorf("cohere embed: got %d vectors for %d texts", len(got), len(batch))
		}
		for _, v := range got {
			if len(v) != Dimension {
				return nil, fmt.Errorf("cohere embed: got a %d-dimension vector, want %d", len(v), Dimension)
			}
		}
		vectors = append(vectors, got...)
	}
	return vectors, nil
}

func (e *CohereEmbedder) call(ctx context.Context, body cohereRequest) ([][]float32, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/v2/embed", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request cohere: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var errBody cohereErrorBody
		_ = json.Unmarshal(respBody, &errBody) // best-effort; the status code is what matters
		return nil, &CohereAPIError{StatusCode: resp.StatusCode, Message: errBody.Message}
	}
	var result cohereResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result.Embeddings.Float, nil
}
