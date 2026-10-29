// Package api exposes the knowledge service over HTTP. Every /v1 route needs a
// signed token (see package auth); the tenant a call may touch comes from the
// token, never from the request body.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/chxragsaxena/pullwise-knowledge/internal/auth"
	"github.com/chxragsaxena/pullwise-knowledge/internal/chunk"
	"github.com/chxragsaxena/pullwise-knowledge/internal/embed"
	"github.com/chxragsaxena/pullwise-knowledge/internal/store"
)

// Limits protect the service from oversized or abusive requests.
const (
	maxIndexBody  = 16 << 20 // bytes
	maxSearchBody = 64 << 10
	maxFiles      = 1000
	maxFileBytes  = 200_000
	maxChunks     = 3000
	maxPathLen    = 500
	maxQueryChars = 4000
	defaultTopK   = 5
	maxTopK       = 20
	maxExclude    = 100
)

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	refPattern  = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// Store is what the handlers need from persistence.
type Store interface {
	IndexedRef(ctx context.Context, tenant, repo string) (string, bool, error)
	ReplaceRepo(ctx context.Context, tenant, repo, ref string, chunks []store.Stored) error
	Search(ctx context.Context, tenant, repo string, query []float32, topK int, excludePaths []string) ([]store.Match, error)
	DeleteRepo(ctx context.Context, tenant, repo string) (bool, error)
	Ping(ctx context.Context) error
}

// API serves the knowledge endpoints.
type API struct {
	store    Store
	embedder embed.Embedder
	verifier *auth.Verifier
	log      *slog.Logger
}

// New builds an API.
func New(s Store, e embed.Embedder, v *auth.Verifier, log *slog.Logger) *API {
	return &API{store: s, embedder: e, verifier: v, log: log}
}

// Handler returns the routes wrapped in request-id, panic-recovery and logging.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", a.ready)

	index := a.verifier.Require(auth.ScopeIndex, writeError)
	search := a.verifier.Require(auth.ScopeSearch, writeError)
	mux.Handle("POST /v1/index", index(http.HandlerFunc(a.indexRepo)))
	mux.Handle("DELETE /v1/repos/{owner}/{name}", index(http.HandlerFunc(a.deleteRepo)))
	mux.Handle("GET /v1/repos/{owner}/{name}", search(http.HandlerFunc(a.repoStatus)))
	mux.Handle("POST /v1/search", search(http.HandlerFunc(a.searchRepo)))

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No such route.")
	})
	return a.withRequestID(a.withRecovery(a.withLogging(mux)))
}

// ready reports whether the database is reachable. The reason is not exposed.
func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.Ping(ctx); err != nil {
		a.log.Warn("not ready", "error", err)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "The service is not ready.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type indexRequest struct {
	Repo  string      `json:"repo"`
	Ref   string      `json:"ref"`
	Files []fileInput `json:"files"`
}

type fileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type indexResponse struct {
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Status string `json:"status"` // "indexed", or "unchanged" if this ref was already indexed
	Files  int    `json:"files"`
	Chunks int    `json:"chunks"`
}

func (a *API) indexRepo(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.FromContext(r.Context())
	var req indexRequest
	if !decode(w, r, maxIndexBody, &req) {
		return
	}
	if !repoPattern.MatchString(req.Repo) {
		writeError(w, http.StatusBadRequest, "invalid_request", `"repo" must look like owner/name.`)
		return
	}
	if !refPattern.MatchString(req.Ref) {
		writeError(w, http.StatusBadRequest, "invalid_request", `"ref" must be a commit or branch name.`)
		return
	}
	if len(req.Files) == 0 || len(req.Files) > maxFiles {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf(`"files" must hold between 1 and %d files.`, maxFiles))
		return
	}

	// Already indexed at this commit: nothing to do (and no embedding calls spent).
	if ref, ok, err := a.store.IndexedRef(r.Context(), claims.Tenant, req.Repo); err != nil {
		a.internalError(w, r, "read indexed ref", err)
		return
	} else if ok && ref == req.Ref {
		writeJSON(w, http.StatusOK, indexResponse{Repo: req.Repo, Ref: req.Ref, Status: "unchanged"})
		return
	}

	var chunks []chunk.Chunk
	for _, f := range req.Files {
		if f.Path == "" || len(f.Path) > maxPathLen {
			writeError(w, http.StatusBadRequest, "invalid_request", `Every file needs a "path" of up to 500 characters.`)
			return
		}
		if len(f.Content) > maxFileBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("%s is larger than %d bytes.", f.Path, maxFileBytes))
			return
		}
		chunks = append(chunks, chunk.Split(f.Path, f.Content)...)
		if len(chunks) > maxChunks {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("The files make more than %d chunks.", maxChunks))
			return
		}
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text()
	}
	vectors, err := a.embedder.Embed(r.Context(), texts, embed.Document)
	if err != nil {
		a.log.Error("embedding failed", "request_id", requestID(r), "error", err)
		writeError(w, http.StatusBadGateway, "embedding_unavailable", "The embedding provider could not be reached.")
		return
	}

	stored := make([]store.Stored, len(chunks))
	for i := range chunks {
		stored[i] = store.Stored{Chunk: chunks[i], Vector: vectors[i]}
	}
	if err := a.store.ReplaceRepo(r.Context(), claims.Tenant, req.Repo, req.Ref, stored); err != nil {
		a.internalError(w, r, "store chunks", err)
		return
	}
	a.log.Info("indexed", "request_id", requestID(r), "tenant", claims.Tenant, "repo", req.Repo,
		"ref", req.Ref, "files", len(req.Files), "chunks", len(chunks))
	writeJSON(w, http.StatusOK, indexResponse{Repo: req.Repo, Ref: req.Ref, Status: "indexed", Files: len(req.Files), Chunks: len(chunks)})
}

type searchRequest struct {
	Repo         string   `json:"repo"`
	Query        string   `json:"query"`
	TopK         int      `json:"topK"`
	ExcludePaths []string `json:"excludePaths"`
}

type searchResult struct {
	Path      string  `json:"path"`
	StartLine int     `json:"startLine"`
	EndLine   int     `json:"endLine"`
	Content   string  `json:"content"`
	Score     float64 `json:"score"`
}

func (a *API) searchRepo(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.FromContext(r.Context())
	var req searchRequest
	if !decode(w, r, maxSearchBody, &req) {
		return
	}
	if !repoPattern.MatchString(req.Repo) {
		writeError(w, http.StatusBadRequest, "invalid_request", `"repo" must look like owner/name.`)
		return
	}
	if req.Query == "" || len(req.Query) > maxQueryChars {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf(`"query" must be 1 to %d characters.`, maxQueryChars))
		return
	}
	if req.TopK == 0 {
		req.TopK = defaultTopK
	}
	if req.TopK < 1 || req.TopK > maxTopK {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf(`"topK" must be between 1 and %d.`, maxTopK))
		return
	}
	if len(req.ExcludePaths) > maxExclude {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf(`"excludePaths" may hold at most %d paths.`, maxExclude))
		return
	}

	vectors, err := a.embedder.Embed(r.Context(), []string{req.Query}, embed.Query)
	if err != nil || len(vectors) != 1 {
		a.log.Error("query embedding failed", "request_id", requestID(r), "error", err)
		writeError(w, http.StatusBadGateway, "embedding_unavailable", "The embedding provider could not be reached.")
		return
	}
	matches, err := a.store.Search(r.Context(), claims.Tenant, req.Repo, vectors[0], req.TopK, req.ExcludePaths)
	if err != nil {
		a.internalError(w, r, "search", err)
		return
	}

	results := make([]searchResult, 0, len(matches))
	for _, m := range matches {
		results = append(results, searchResult{Path: m.Path, StartLine: m.StartLine, EndLine: m.EndLine, Content: m.Content, Score: m.Score})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// repoStatus tells a caller which commit of a repo is indexed for its tenant, so
// it can skip downloading and re-sending a repository that is already up to date.
func (a *API) repoStatus(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.FromContext(r.Context())
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	if !repoPattern.MatchString(repo) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The repo must look like owner/name.")
		return
	}
	ref, ok, err := a.store.IndexedRef(r.Context(), claims.Tenant, repo)
	if err != nil {
		a.internalError(w, r, "read indexed ref", err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "That repository is not indexed.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"repo": repo, "ref": ref})
}

func (a *API) deleteRepo(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.FromContext(r.Context())
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	if !repoPattern.MatchString(repo) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The repo must look like owner/name.")
		return
	}
	deleted, err := a.store.DeleteRepo(r.Context(), claims.Tenant, repo)
	if err != nil {
		a.internalError(w, r, "delete repo", err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "not_found", "That repository is not indexed.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decode reads a size-limited JSON body, rejecting unknown fields. Rejecting
// unknown fields matters here: a caller that sends a "tenant" in the body gets
// an error instead of having it silently ignored.
func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("The request body is larger than %d bytes.", limit))
		default:
			writeError(w, http.StatusBadRequest, "invalid_request", "The request body is not valid JSON for this endpoint.")
		}
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body must hold a single JSON object.")
		return false
	}
	return true
}

func (a *API) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.log.Error(what, "request_id", requestID(r), "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
}

// --- responses ---

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, message
	writeJSON(w, status, b)
}

// --- middleware ---

type requestIDKey struct{}

func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

// withRequestID keeps the caller's X-Request-ID (if it's well-formed) or makes
// one, returns it in the response and puts it in the context, so one request
// can be followed across services in the logs.
func (a *API) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !idPattern.MatchString(id) {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func (a *API) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				a.log.Error("panic", "request_id", requestID(r), "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (a *API) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Never log headers or bodies: they carry tokens and source code.
		a.log.Info("request", "request_id", requestID(r), "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start))
	})
}
