package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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

// limited answers the first n requests with 429 (and retryAfter, if set), then succeeds.
func limited(t *testing.T, n int32, retryAfter string) (*CohereEmbedder, *atomic.Int32, *[]time.Duration) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= n {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"trial token rate limit exceeded, limit is 100000 tokens per minute"}`)
			return
		}
		vectors := [][]float32{make([]float32, Dimension)}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": map[string]any{"float": vectors}})
	}))
	t.Cleanup(srv.Close)

	var slept []time.Duration
	e := NewCohereEmbedder("k", WithCohereBaseURL(srv.URL))
	e.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	return e, &hits, &slept
}

func TestARateLimitedBatchWaitsAndRetries(t *testing.T) {
	e, hits, slept := limited(t, 2, "")

	got, err := e.Embed(context.Background(), texts(1), Document)
	if err != nil || len(got) != 1 {
		t.Fatalf("Embed = %d vectors, %v", len(got), err)
	}
	if hits.Load() != 3 || len(*slept) != 2 || (*slept)[0] != defaultRateLimitWait {
		t.Fatalf("hits=%d waits=%v, want 3 requests and two default waits", hits.Load(), *slept)
	}
}

func TestTheWaitCohereAsksForIsUsedButCapped(t *testing.T) {
	e, _, slept := limited(t, 1, "12")
	if _, err := e.Embed(context.Background(), texts(1), Document); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 12*time.Second {
		t.Fatalf("waited %v, want the 12s Cohere asked for", *slept)
	}

	e, _, slept = limited(t, 1, "3600")
	_, _ = e.Embed(context.Background(), texts(1), Document)
	if len(*slept) != 1 || (*slept)[0] != maxRateLimitWait {
		t.Fatalf("waited %v, want the wait capped at %s", *slept, maxRateLimitWait)
	}
}

func TestItGivesUpAfterTheRetriesAndKeepsTheRateLimitError(t *testing.T) {
	e, hits, _ := limited(t, 100, "")

	_, err := e.Embed(context.Background(), texts(1), Document)
	var apiErr *CohereAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 {
		t.Fatalf("error = %v, want the 429", err)
	}
	if hits.Load() != maxRateLimitRetries+1 {
		t.Fatalf("made %d requests, want the first plus %d retries", hits.Load(), maxRateLimitRetries)
	}
}

func TestCancellingDuringTheWaitStopsRetrying(t *testing.T) {
	e, hits, _ := limited(t, 100, "")
	e.sleep = func(context.Context, time.Duration) error { return context.Canceled }

	if _, err := e.Embed(context.Background(), texts(1), Document); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("made %d requests after cancellation", hits.Load())
	}
}

func TestEmbedReturnsTheAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"upstream trouble"}`)
	}))
	t.Cleanup(srv.Close)
	e := NewCohereEmbedder("k", WithCohereBaseURL(srv.URL))
	e.sleep = func(context.Context, time.Duration) error {
		t.Error("slept for an error that is not a rate limit")
		return nil
	}

	_, err := e.Embed(context.Background(), texts(3), Document)
	var apiErr *CohereAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 || apiErr.Message != "upstream trouble" {
		t.Fatalf("error = %v, want the 500 CohereAPIError with its message", err)
	}
}
