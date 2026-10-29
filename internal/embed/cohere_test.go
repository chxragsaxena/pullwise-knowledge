package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fakeCohere(t *testing.T, dim int, calls *[]cohereRequest) *CohereEmbedder {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.URL.Path != "/v2/embed" {
			t.Errorf("unexpected request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var req cohereRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		*calls = append(*calls, req)
		vectors := make([][]float32, len(req.Texts))
		for i := range vectors {
			vectors[i] = make([]float32, dim)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": map[string]any{"float": vectors}})
	}))
	t.Cleanup(srv.Close)
	return NewCohereEmbedder("test-key", WithCohereBaseURL(srv.URL))
}

func texts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("t%d", i)
	}
	return out
}

func TestEmbedBatchesAtTheLimitAndAsksForTheStoredDimension(t *testing.T) {
	tests := []struct {
		n         int
		wantSizes []int
	}{{0, nil}, {1, []int{1}}, {96, []int{96}}, {97, []int{96, 1}}, {200, []int{96, 96, 8}}}
	for _, tt := range tests {
		var calls []cohereRequest
		e := fakeCohere(t, Dimension, &calls)

		got, err := e.Embed(context.Background(), texts(tt.n), Document)
		if err != nil || len(got) != tt.n {
			t.Fatalf("n=%d: %d vectors, err %v", tt.n, len(got), err)
		}
		var sizes []int
		for _, c := range calls {
			sizes = append(sizes, len(c.Texts))
			if c.OutputDimension != Dimension {
				t.Errorf("output_dimension = %d, want %d", c.OutputDimension, Dimension)
			}
		}
		if fmt.Sprint(sizes) != fmt.Sprint(tt.wantSizes) {
			t.Errorf("n=%d: call sizes %v, want %v", tt.n, sizes, tt.wantSizes)
		}
	}
}

func TestEmbedUsesDifferentModelsForDocumentsAndQueries(t *testing.T) {
	var calls []cohereRequest
	e := fakeCohere(t, Dimension, &calls)
	_, _ = e.Embed(context.Background(), texts(1), Document)
	_, _ = e.Embed(context.Background(), texts(1), Query)

	if calls[0].Model != DefaultCohereDocumentModel || calls[0].InputType != "search_document" {
		t.Errorf("document call = %+v", calls[0])
	}
	if calls[1].Model != DefaultCohereQueryModel || calls[1].InputType != "search_query" {
		t.Errorf("query call = %+v", calls[1])
	}
}

func TestEmbedRejectsAVectorOfTheWrongSize(t *testing.T) {
	var calls []cohereRequest
	e := fakeCohere(t, 768, &calls) // e.g. a mis-configured model
	if _, err := e.Embed(context.Background(), texts(1), Document); err == nil {
		t.Fatal("a 768-dimension vector must not be accepted for a 1024-dimension column")
	}
}

func TestEmbedReturnsTheAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":"too many requests"}`)
	}))
	t.Cleanup(srv.Close)
	e := NewCohereEmbedder("k", WithCohereBaseURL(srv.URL))

	_, err := e.Embed(context.Background(), texts(3), Document)
	var apiErr *CohereAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 {
		t.Fatalf("error = %v, want a 429 CohereAPIError", err)
	}
}
