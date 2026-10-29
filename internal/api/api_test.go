package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chxragsaxena/pullwise-knowledge/internal/auth"
	"github.com/chxragsaxena/pullwise-knowledge/internal/embed"
	"github.com/chxragsaxena/pullwise-knowledge/internal/store"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

// fakeStore keeps chunks in memory, keyed by tenant and repo.
type fakeStore struct {
	mu      sync.Mutex
	repos   map[string]map[string][]store.Stored // tenant -> repo -> chunks
	refs    map[string]string                    // tenant|repo -> ref
	pingErr error
	failOn  string
	calls   []string // tenant seen by each store call
}

func newFakeStore() *fakeStore {
	return &fakeStore{repos: map[string]map[string][]store.Stored{}, refs: map[string]string{}}
}

func (f *fakeStore) note(tenant string) {
	f.mu.Lock()
	f.calls = append(f.calls, tenant)
	f.mu.Unlock()
}

func (f *fakeStore) IndexedRef(_ context.Context, tenant, repo string) (string, bool, error) {
	f.note(tenant)
	ref, ok := f.refs[tenant+"|"+repo]
	return ref, ok, nil
}

func (f *fakeStore) ReplaceRepo(_ context.Context, tenant, repo, ref string, chunks []store.Stored) error {
	f.note(tenant)
	if f.failOn == "replace" {
		return errors.New("disk full")
	}
	if f.repos[tenant] == nil {
		f.repos[tenant] = map[string][]store.Stored{}
	}
	f.repos[tenant][repo] = chunks
	f.refs[tenant+"|"+repo] = ref
	return nil
}

func (f *fakeStore) Search(_ context.Context, tenant, repo string, _ []float32, topK int, exclude []string) ([]store.Match, error) {
	f.note(tenant)
	var out []store.Match
	for _, c := range f.repos[tenant][repo] {
		skip := false
		for _, e := range exclude {
			if e == c.Path {
				skip = true
			}
		}
		if !skip {
			out = append(out, store.Match{Path: c.Path, StartLine: c.StartLine, EndLine: c.EndLine, Content: c.Content, Score: 0.9})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func (f *fakeStore) DeleteRepo(_ context.Context, tenant, repo string) (bool, error) {
	f.note(tenant)
	_, ok := f.repos[tenant][repo]
	delete(f.repos[tenant], repo)
	return ok, nil
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

type fakeEmbedder struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, _ embed.Kind) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, embed.Dimension)
	}
	return out, nil
}

type harness struct {
	h  http.Handler
	st *fakeStore
	em *fakeEmbedder
}

func newHarness(t *testing.T) harness {
	t.Helper()
	v, err := auth.NewVerifier(secret, "pullwise", "pullwise-knowledge")
	if err != nil {
		t.Fatal(err)
	}
	st, em := newFakeStore(), &fakeEmbedder{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return harness{h: New(st, em, v, log).Handler(), st: st, em: em}
}

func token(t *testing.T, tenant string, scopes ...string) string {
	t.Helper()
	tok, err := auth.Issue(secret, "pullwise", "pullwise-knowledge", tenant, scopes, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (h harness) do(method, path, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	if err := json.NewDecoder(rec.Body).Decode(&b); err != nil {
		t.Fatalf("not an error body: %q", rec.Body.String())
	}
	return b.Error.Code
}

const indexBody = `{"repo":"o/r","ref":"abc123","files":[
	{"path":"a.go","content":"package a\nfunc A() {}\n"},
	{"path":"b.py","content":"def b():\n    pass\n"}]}`

func TestRoutesNeedTheRightToken(t *testing.T) {
	h := newHarness(t)
	routes := []struct {
		method, path, body, scope string
	}{
		{"POST", "/v1/index", indexBody, auth.ScopeIndex},
		{"POST", "/v1/search", `{"repo":"o/r","query":"x"}`, auth.ScopeSearch},
		{"DELETE", "/v1/repos/o/r", "", auth.ScopeIndex},
	}
	for _, rt := range routes {
		other := auth.ScopeSearch
		if rt.scope == auth.ScopeSearch {
			other = auth.ScopeIndex
		}
		if rec := h.do(rt.method, rt.path, rt.body, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", rt.method, rt.path, rec.Code)
		}
		if rec := h.do(rt.method, rt.path, rt.body, "garbage"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a bad token = %d, want 401", rt.method, rt.path, rec.Code)
		}
		if rec := h.do(rt.method, rt.path, rt.body, token(t, "u1", other)); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with the wrong scope = %d, want 403", rt.method, rt.path, rec.Code)
		}
	}
	if len(h.st.calls) != 0 || h.em.calls != 0 {
		t.Fatal("a rejected request reached the store or the embedding provider")
	}
}

func TestHealthIsOpenAndReadinessHidesTheReason(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("GET", "/healthz", "", ""); rec.Code != 200 {
		t.Fatalf("/healthz = %d", rec.Code)
	}
	if rec := h.do("GET", "/readyz", "", ""); rec.Code != 200 {
		t.Fatalf("/readyz = %d", rec.Code)
	}
	h.st.pingErr = errors.New("password authentication failed for user x")
	rec := h.do("GET", "/readyz", "", "")
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("/readyz = %d %s; want 503 without the internal reason", rec.Code, rec.Body.String())
	}
}

func TestIndexThenSearchUsesTheTokensTenant(t *testing.T) {
	h := newHarness(t)

	rec := h.do("POST", "/v1/index", indexBody, token(t, "u1", auth.ScopeIndex))
	if rec.Code != 200 {
		t.Fatalf("index = %d %s", rec.Code, rec.Body.String())
	}
	var ir indexResponse
	_ = json.NewDecoder(rec.Body).Decode(&ir)
	if ir.Status != "indexed" || ir.Files != 2 || ir.Chunks < 2 {
		t.Fatalf("index response = %+v", ir)
	}

	// The same repo name under another tenant has nothing indexed.
	rec = h.do("POST", "/v1/search", `{"repo":"o/r","query":"retry"}`, token(t, "u2", auth.ScopeSearch))
	var sr struct{ Results []searchResult }
	_ = json.NewDecoder(rec.Body).Decode(&sr)
	if rec.Code != 200 || len(sr.Results) != 0 {
		t.Fatalf("another tenant's search = %d %+v, want 200 and no results", rec.Code, sr)
	}

	rec = h.do("POST", "/v1/search", `{"repo":"o/r","query":"retry","topK":1}`, token(t, "u1", auth.ScopeSearch))
	sr.Results = nil
	_ = json.NewDecoder(rec.Body).Decode(&sr)
	if rec.Code != 200 || len(sr.Results) != 1 || sr.Results[0].Path != "a.go" {
		t.Fatalf("owner's search = %d %+v", rec.Code, sr)
	}

	for _, tenant := range h.st.calls {
		if tenant != "u1" && tenant != "u2" {
			t.Fatalf("store was called for tenant %q", tenant)
		}
	}
}

func TestATenantInTheBodyIsRefusedNotIgnored(t *testing.T) {
	h := newHarness(t)
	body := `{"repo":"o/r","query":"x","tenant":"someone-else"}`
	rec := h.do("POST", "/v1/search", body, token(t, "u1", auth.ScopeSearch))
	if rec.Code != 400 || errorCode(t, rec) != "invalid_request" {
		t.Fatalf("status = %d, want 400 invalid_request", rec.Code)
	}
}

func TestIndexIsSkippedWhenTheRefIsAlreadyIndexed(t *testing.T) {
	h := newHarness(t)
	tok := token(t, "u1", auth.ScopeIndex)
	h.do("POST", "/v1/index", indexBody, tok)
	callsAfterFirst := h.em.calls

	rec := h.do("POST", "/v1/index", indexBody, tok)
	var ir indexResponse
	_ = json.NewDecoder(rec.Body).Decode(&ir)
	if rec.Code != 200 || ir.Status != "unchanged" {
		t.Fatalf("second index = %d %+v, want unchanged", rec.Code, ir)
	}
	if h.em.calls != callsAfterFirst {
		t.Fatal("re-indexing an unchanged ref spent embedding calls")
	}
}

func TestIndexValidation(t *testing.T) {
	big := strings.Repeat("x", maxFileBytes+1)
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"not json", `nope`, 400, "invalid_request"},
		{"two objects", `{"repo":"o/r","ref":"a","files":[{"path":"a","content":"x"}]}{}`, 400, "invalid_request"},
		{"bad repo", `{"repo":"nope","ref":"a","files":[{"path":"a","content":"x"}]}`, 400, "invalid_request"},
		{"repo with traversal", `{"repo":"../x/y","ref":"a","files":[{"path":"a","content":"x"}]}`, 400, "invalid_request"},
		{"bad ref", `{"repo":"o/r","ref":"has space","files":[{"path":"a","content":"x"}]}`, 400, "invalid_request"},
		{"no files", `{"repo":"o/r","ref":"a","files":[]}`, 400, "invalid_request"},
		{"empty path", `{"repo":"o/r","ref":"a","files":[{"path":"","content":"x"}]}`, 400, "invalid_request"},
		{"file too large", `{"repo":"o/r","ref":"a","files":[{"path":"a.go","content":"` + big + `"}]}`, 413, "too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do("POST", "/v1/index", tt.body, token(t, "u1", auth.ScopeIndex))
			if rec.Code != tt.wantStatus || errorCode(t, rec) != tt.wantCode {
				t.Fatalf("status = %d (%s), want %d %s", rec.Code, rec.Body.String(), tt.wantStatus, tt.wantCode)
			}
			if h.em.calls != 0 || len(h.st.repos) != 0 {
				t.Fatal("an invalid request was embedded or stored")
			}
		})
	}
}

func TestIndexRejectsTooManyChunksAndTooLargeABody(t *testing.T) {
	h := newHarness(t)
	tok := token(t, "u1", auth.ScopeIndex)

	// Many small files, each its own chunk.
	var files []string
	for i := 0; i < maxChunks+1; i++ {
		files = append(files, `{"path":"f`+string(rune('a'+i%26))+strings.Repeat("x", i%7)+`.py","content":"a = 1\n"}`)
	}
	rec := h.do("POST", "/v1/index", `{"repo":"o/r","ref":"a","files":[`+strings.Join(files, ",")+`]}`, tok)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too many files/chunks = %d, want a rejection", rec.Code)
	}

	huge := `{"repo":"o/r","ref":"a","files":[{"path":"a","content":"` + strings.Repeat("y", maxIndexBody) + `"}]}`
	rec = h.do("POST", "/v1/index", huge, tok)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "too_large" {
		t.Fatalf("oversized body = %d, want 413 too_large", rec.Code)
	}
	if h.em.calls != 0 {
		t.Fatal("an oversized request was embedded")
	}
}

func TestSearchValidation(t *testing.T) {
	tests := []struct{ name, body string }{
		{"bad repo", `{"repo":"x","query":"q"}`},
		{"empty query", `{"repo":"o/r","query":""}`},
		{"query too long", `{"repo":"o/r","query":"` + strings.Repeat("q", maxQueryChars+1) + `"}`},
		{"topK too big", `{"repo":"o/r","query":"q","topK":500}`},
		{"negative topK", `{"repo":"o/r","query":"q","topK":-1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do("POST", "/v1/search", tt.body, token(t, "u1", auth.ScopeSearch))
			if rec.Code != 400 || errorCode(t, rec) != "invalid_request" {
				t.Fatalf("status = %d %s, want 400 invalid_request", rec.Code, rec.Body.String())
			}
			if h.em.calls != 0 {
				t.Fatal("an invalid search spent an embedding call")
			}
		})
	}
}

func TestSearchCanExcludePaths(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/v1/index", indexBody, token(t, "u1", auth.ScopeIndex))

	rec := h.do("POST", "/v1/search", `{"repo":"o/r","query":"q","excludePaths":["a.go"]}`, token(t, "u1", auth.ScopeSearch))
	var sr struct{ Results []searchResult }
	_ = json.NewDecoder(rec.Body).Decode(&sr)
	for _, r := range sr.Results {
		if r.Path == "a.go" {
			t.Fatal("an excluded path was returned")
		}
	}
	if len(sr.Results) == 0 {
		t.Fatal("expected the other file's chunks")
	}
}

func TestProviderAndStoreFailuresAreReportedWithoutLeakingDetails(t *testing.T) {
	tok := token(t, "u1", auth.ScopeIndex)

	h := newHarness(t)
	h.em.err = errors.New("401 invalid api key sk-secret")
	rec := h.do("POST", "/v1/index", indexBody, tok)
	if rec.Code != 502 || errorCode(t, rec) != "embedding_unavailable" || strings.Contains(rec.Body.String(), "sk-secret") {
		t.Fatalf("embedding failure = %d %s", rec.Code, rec.Body.String())
	}
	if len(h.st.repos) != 0 {
		t.Fatal("chunks were stored although embedding failed")
	}

	h = newHarness(t)
	h.st.failOn = "replace"
	rec = h.do("POST", "/v1/index", indexBody, tok)
	if rec.Code != 500 || errorCode(t, rec) != "internal_error" || strings.Contains(rec.Body.String(), "disk full") {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRepoStatusReportsTheIndexedRefPerTenant(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/v1/index", indexBody, token(t, "u1", auth.ScopeIndex))

	rec := h.do("GET", "/v1/repos/o/r", "", token(t, "u1", auth.ScopeSearch))
	var got map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if rec.Code != 200 || got["ref"] != "abc123" {
		t.Fatalf("owner's status = %d %v", rec.Code, got)
	}
	if rec := h.do("GET", "/v1/repos/o/r", "", token(t, "u2", auth.ScopeSearch)); rec.Code != 404 {
		t.Fatalf("another tenant's status = %d, want 404", rec.Code)
	}
	if rec := h.do("GET", "/v1/repos/o/r", "", ""); rec.Code != 401 {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
	if rec := h.do("GET", "/v1/repos/o/r", "", token(t, "u1", auth.ScopeIndex)); rec.Code != 403 {
		t.Fatalf("index-only token = %d, want 403 (status needs the search scope)", rec.Code)
	}
}

func TestDeleteRepo(t *testing.T) {
	h := newHarness(t)
	idx, del := token(t, "u1", auth.ScopeIndex), token(t, "u2", auth.ScopeIndex)
	h.do("POST", "/v1/index", indexBody, idx)

	if rec := h.do("DELETE", "/v1/repos/o/r", "", del); rec.Code != 404 {
		t.Fatalf("another tenant deleting = %d, want 404 (it has no such repo)", rec.Code)
	}
	if rec := h.do("DELETE", "/v1/repos/o/r", "", idx); rec.Code != 204 {
		t.Fatalf("owner deleting = %d, want 204", rec.Code)
	}
	if rec := h.do("DELETE", "/v1/repos/o/r", "", idx); rec.Code != 404 {
		t.Fatalf("deleting twice = %d, want 404", rec.Code)
	}
}

func TestRequestIDIsKeptWhenWellFormedAndReplacedWhenNot(t *testing.T) {
	h := newHarness(t)
	for _, tt := range []struct {
		sent     string
		wantSame bool
	}{{"trace-123", true}, {"bad id with spaces\n", false}, {"", false}} {
		req := httptest.NewRequest("GET", "/healthz", nil)
		if tt.sent != "" {
			req.Header.Set("X-Request-ID", tt.sent)
		}
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		got := rec.Header().Get("X-Request-ID")
		if got == "" || (got == tt.sent) != tt.wantSame {
			t.Errorf("sent %q, got %q", tt.sent, got)
		}
	}
}

func TestUnknownRouteIs404InTheStandardShape(t *testing.T) {
	rec := newHarness(t).do("GET", "/v2/whatever", "", "")
	if rec.Code != 404 || errorCode(t, rec) != "not_found" {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
}
