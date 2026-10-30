package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/chxragsaxena/pullwise-knowledge/internal/chunk"
	"github.com/chxragsaxena/pullwise-knowledge/internal/embed"
	"github.com/chxragsaxena/pullwise-knowledge/migrations"
)

// testStore needs a throwaway *_test database in TEST_DATABASE_URL (its schema is wiped).
func testStore(t *testing.T) *Store {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_test") {
		t.Fatalf("TEST_DATABASE_URL must point at a database whose name ends in _test (it is wiped)")
	}
	ctx := context.Background()
	db, err := Open(ctx, raw, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(db)
}

// vec returns a vector with the given components at the start, zero elsewhere.
func vec(components ...float32) []float32 {
	v := make([]float32, embed.Dimension)
	copy(v, components)
	return v
}

func stored(path string, v []float32) Stored {
	return Stored{Chunk: chunk.Chunk{Path: path, StartLine: 1, EndLine: 5, Content: "code of " + path}, Vector: v}
}

func paths(ms []Match) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Path)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWithSearchPath(t *testing.T) {
	got, err := withSearchPath("postgres://u:p%40ss@h:5432/db?sslmode=disable", "knowledge")
	if err != nil || got != "postgres://u:p%40ss@h:5432/db?search_path=knowledge%2Cpublic&sslmode=disable" {
		t.Fatalf("withSearchPath = %q, %v", got, err)
	}
	if _, err := withSearchPath("host=h dbname=db", "knowledge"); err == nil {
		t.Fatal("a non-URL connection string must be rejected")
	}
}

func TestOpenRejectsAnUnsafeSchemaName(t *testing.T) {
	for _, bad := range []string{"Knowledge", "a-b", `x"; DROP SCHEMA public; --`, "1abc", strings.Repeat("a", 64)} {
		if _, err := Open(context.Background(), "postgres://u:p@localhost/db", bad); err == nil || !strings.Contains(err.Error(), "DATABASE_SCHEMA") {
			t.Errorf("schema %q: error = %v, want it rejected before connecting", bad, err)
		}
	}
}

func TestASchemaKeepsTheServicesTablesAwayFromTheOthers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Another application's table of the same name as ours, in public.
	if _, err := s.db.ExecContext(ctx, "DROP TABLE schema_migrations CASCADE; DROP TABLE chunks CASCADE; DROP TABLE indexed_repos CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, other TEXT)"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(ctx, "DROP SCHEMA IF EXISTS kb CASCADE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.db.Exec("DROP SCHEMA IF EXISTS kb CASCADE") })

	db, err := Open(ctx, os.Getenv("TEST_DATABASE_URL"), "kb")
	if err != nil {
		t.Fatalf("Open with a schema: %v", err)
	}
	defer db.Close()
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatalf("Migrate inside the schema: %v", err)
	}

	var inKB, inPublic int
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'kb' AND table_name IN ('chunks','indexed_repos','schema_migrations')").Scan(&inKB)
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('chunks','indexed_repos')").Scan(&inPublic)
	if inKB != 3 || inPublic != 0 {
		t.Fatalf("tables in kb = %d (want 3), chunk tables left in public = %d (want 0)", inKB, inPublic)
	}

	ks := New(db)
	if err := ks.ReplaceRepo(ctx, "u1", "o/r", "x", []Stored{stored("a.go", vec(1))}); err != nil {
		t.Fatal(err)
	}
	if got, err := ks.Search(ctx, "u1", "o/r", vec(1), "", 3, nil); err != nil || len(got) != 1 {
		t.Fatalf("search inside the schema = %v, %v", got, err)
	}
}

func TestCheckSchemaRefusesAConnectionThatIgnoredTheSchema(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := checkSchema(ctx, s.db, "public"); err != nil {
		t.Fatalf("matching schema rejected: %v", err)
	}
	err := checkSchema(ctx, s.db, "knowledge")
	if err == nil || !strings.Contains(err.Error(), "pooled") {
		t.Fatalf("error = %v, want it to say the connection is probably pooled", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := testStore(t)
	if err := Migrate(context.Background(), s.db, migrations.Files); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestSearchRanksByCosineSimilarity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	err := s.ReplaceRepo(ctx, "u1", "o/r", "sha1", []Stored{
		stored("far.go", vec(0, 1)),
		stored("near.go", vec(1, 0.1)),
		stored("same.go", vec(1, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Search(ctx, "u1", "o/r", vec(1, 0), "", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"same.go", "near.go", "far.go"}; !equal(paths(got), want) {
		t.Fatalf("order = %v, want %v", paths(got), want)
	}
	if got[0].Score < 0.999 || got[2].Score > 0.001 {
		t.Fatalf("scores = %v .. %v, want ~1 for identical and ~0 for orthogonal", got[0].Score, got[2].Score)
	}

	top1, _ := s.Search(ctx, "u1", "o/r", vec(1, 0), "", 1, nil)
	if len(top1) != 1 || top1[0].Path != "same.go" {
		t.Fatalf("topK=1 returned %v", paths(top1))
	}
}

func TestTenantsCannotSeeEachOthersChunks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Same repo name and vector under two tenants: ignoring the tenant would return both.
	if err := s.ReplaceRepo(ctx, "u1", "o/secret", "a", []Stored{stored("alice.go", vec(1))}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceRepo(ctx, "u2", "o/secret", "b", []Stored{stored("bob.go", vec(1))}); err != nil {
		t.Fatal(err)
	}

	for tenant, want := range map[string]string{"u1": "alice.go", "u2": "bob.go"} {
		got, err := s.Search(ctx, tenant, "o/secret", vec(1), "", 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Path != want {
			t.Errorf("tenant %s saw %v, want only %s", tenant, paths(got), want)
		}
	}

	if got, _ := s.Search(ctx, "u3", "o/secret", vec(1), "", 10, nil); len(got) != 0 {
		t.Errorf("a tenant with no index saw %v", paths(got))
	}
	if ref, ok, _ := s.IndexedRef(ctx, "u3", "o/secret"); ok {
		t.Errorf("a tenant with no index reported ref %q", ref)
	}

	// One tenant deleting its repo must not touch the other's.
	if deleted, err := s.DeleteRepo(ctx, "u1", "o/secret"); err != nil || !deleted {
		t.Fatalf("DeleteRepo = %v, %v", deleted, err)
	}
	if got, _ := s.Search(ctx, "u2", "o/secret", vec(1), "", 10, nil); len(got) != 1 {
		t.Error("deleting tenant u1's index removed tenant u2's chunks")
	}
}

func TestReplaceRepoReplacesEverythingAndRecordsTheRef(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.ReplaceRepo(ctx, "u1", "o/r", "old", []Stored{stored("old.go", vec(1)), stored("gone.go", vec(1))})
	if err := s.ReplaceRepo(ctx, "u1", "o/r", "new", []Stored{stored("new.go", vec(1))}); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Search(ctx, "u1", "o/r", vec(1), "", 10, nil)
	if !equal(paths(got), []string{"new.go"}) {
		t.Fatalf("after replace saw %v, want only new.go", paths(got))
	}
	if ref, ok, _ := s.IndexedRef(ctx, "u1", "o/r"); !ok || ref != "new" {
		t.Fatalf("ref = %q, %v; want new", ref, ok)
	}
}

func TestAFailedReplaceLeavesThePreviousIndexIntact(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.ReplaceRepo(ctx, "u1", "o/r", "good", []Stored{stored("keep.go", vec(1))})

	err := s.ReplaceRepo(ctx, "u1", "o/r", "bad", []Stored{
		stored("fine.go", vec(1)),
		{Chunk: chunk.Chunk{Path: "x.go", StartLine: 1, EndLine: 1, Content: "x"}, Vector: []float32{1, 2, 3}}, // wrong size
	})
	if err == nil {
		t.Fatal("a wrong-size vector must be rejected")
	}

	got, _ := s.Search(ctx, "u1", "o/r", vec(1), "", 10, nil)
	if !equal(paths(got), []string{"keep.go"}) {
		t.Fatalf("after a failed replace saw %v, want the old index", paths(got))
	}
	if ref, _, _ := s.IndexedRef(ctx, "u1", "o/r"); ref != "good" {
		t.Fatalf("ref = %q, want good", ref)
	}
}

func TestSearchCanExcludePathsAndRejectsABadQuery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.ReplaceRepo(ctx, "u1", "o/r", "x", []Stored{stored("a.go", vec(1)), stored("b.go", vec(1, 0.5)), stored("c.go", vec(1, 1))})

	got, err := s.Search(ctx, "u1", "o/r", vec(1), "", 10, []string{"a.go", "c.go"})
	if err != nil || !equal(paths(got), []string{"b.go"}) {
		t.Fatalf("got %v, %v; want only b.go", paths(got), err)
	}
	if _, err := s.Search(ctx, "u1", "o/r", []float32{1, 2}, "", 5, nil); err == nil {
		t.Fatal("a query vector of the wrong size must be rejected")
	}
}

func codeChunk(path, content string, v []float32) Stored {
	return Stored{Chunk: chunk.Chunk{Path: path, StartLine: 1, EndLine: 5, Content: content}, Vector: v}
}

func TestKeywordsRescueAChunkTheVectorsMiss(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// The query vector points at five decoys; only the keywords lead to cohere.go.
	chunks := []Stored{codeChunk("internal/embed/cohere.go", "type CohereEmbedder struct{}", vec(0, 1))}
	for i := 0; i < 5; i++ {
		chunks = append(chunks, codeChunk(fmt.Sprintf("decoy%d.go", i), "package decoy", vec(1, float32(i)*0.01)))
	}
	if err := s.ReplaceRepo(ctx, "u1", "o/r", "x", chunks); err != nil {
		t.Fatal(err)
	}

	without, _ := s.Search(ctx, "u1", "o/r", vec(1), "", 3, nil)
	for _, p := range paths(without) {
		if p == "internal/embed/cohere.go" {
			t.Fatalf("setup is wrong: vector search alone already found it: %v", paths(without))
		}
	}

	with, err := s.Search(ctx, "u1", "o/r", vec(1), "which model does the embedder use", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range paths(with) {
		found = found || p == "internal/embed/cohere.go"
	}
	if !found {
		t.Fatalf("keywords did not bring cohere.go into the top 3: %v", paths(with))
	}
}

func TestKeywordSearchStaysInsideTheTenantAndHonoursExclusions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.ReplaceRepo(ctx, "u1", "o/r", "x", []Stored{codeChunk("secret_token.go", "func SecretToken() {}", vec(0, 1))})
	_ = s.ReplaceRepo(ctx, "u2", "o/r", "x", []Stored{codeChunk("other.go", "package other", vec(1))})

	got, err := s.Search(ctx, "u2", "o/r", vec(1), "secret token", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(paths(got), []string{"other.go"}) {
		t.Fatalf("tenant u2 saw %v through a keyword search, want only its own chunk", paths(got))
	}

	got, _ = s.Search(ctx, "u1", "o/r", vec(1), "secret token", 10, []string{"secret_token.go"})
	if len(got) != 0 {
		t.Fatalf("an excluded path came back through keywords: %v", paths(got))
	}
}

func TestKeywordsThatReduceToNothingFallBackToVectorSearch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.ReplaceRepo(ctx, "u1", "o/r", "x", []Stored{stored("a.go", vec(1))})

	got, err := s.Search(ctx, "u1", "o/r", vec(1), "how does it?? the", 3, nil)
	if err != nil || !equal(paths(got), []string{"a.go"}) {
		t.Fatalf("got %v, %v", paths(got), err)
	}
}

func TestDeleteRepoReportsWhetherAnythingWasDeleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if deleted, err := s.DeleteRepo(ctx, "u1", "o/none"); err != nil || deleted {
		t.Fatalf("deleting a missing repo = %v, %v; want false, nil", deleted, err)
	}
	_ = s.ReplaceRepo(ctx, "u1", "o/r", "x", []Stored{stored("a.go", vec(1))})
	if deleted, _ := s.DeleteRepo(ctx, "u1", "o/r"); !deleted {
		t.Fatal("deleting an existing repo should report true")
	}
	var chunks int
	_ = s.db.QueryRow("SELECT count(*) FROM chunks").Scan(&chunks)
	if chunks != 0 {
		t.Fatalf("%d chunks left behind after deleting the repo", chunks)
	}
}
