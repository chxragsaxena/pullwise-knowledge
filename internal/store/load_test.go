package store

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chxragsaxena/pullwise-knowledge/internal/chunk"
	"github.com/chxragsaxena/pullwise-knowledge/internal/embed"
)

// How the exact (no ANN index) hybrid search scales as one repository's index grows. Needs a pgvector
// database in TEST_DATABASE_URL, and runs only with LOAD=1:  LOAD=1 go test -run Load -v ./internal/store/

func randomVector(r *rand.Rand) []float32 {
	v := make([]float32, embed.Dimension)
	for i := range v {
		v[i] = r.Float32()*2 - 1
	}
	return v
}

// randomWord is a distinct lowercase word; real code mixes rare and common words, so each one is rare here.
func randomWord(r *rand.Rand) string {
	b := make([]byte, 9)
	for i := range b {
		b[i] = byte('a' + r.Intn(26))
	}
	return string(b)
}

func fakeCode(r *rand.Rand, vocab []string) string {
	words := make([]string, 60)
	for i := range words {
		words[i] = vocab[r.Intn(len(vocab))]
	}
	return strings.Join(words, " ")
}

func percentileOf(d []time.Duration, p float64) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[int(float64(len(d)-1)*p)]
}

func TestLoadSearchScaling(t *testing.T) {
	if os.Getenv("LOAD") == "" {
		t.Skip("set LOAD=1 to run load tests")
	}
	s := testStore(t)
	ctx := context.Background()
	r := rand.New(rand.NewSource(1))

	vocab := make([]string, 4000)
	for i := range vocab {
		vocab[i] = randomWord(r)
	}

	have := 0
	for _, size := range []int{5_000, 20_000} {
		// Grow the same repository to the target size, so each step reuses the earlier rows' work.
		all := make([]Stored, 0, size)
		for i := 0; i < size; i++ {
			all = append(all, Stored{
				Chunk:  chunk.Chunk{Path: fmt.Sprintf("pkg%d/file%d.go", i%200, i), StartLine: 1, EndLine: 40, Content: fakeCode(r, vocab)},
				Vector: randomVector(r),
			})
		}
		t0 := time.Now()
		if err := s.ReplaceRepo(ctx, "load", "o/big", fmt.Sprintf("ref%d", size), all); err != nil {
			t.Fatal(err)
		}
		insert := time.Since(t0)
		have = size
		if _, err := s.db.ExecContext(ctx, "ANALYZE chunks"); err != nil {
			t.Fatal(err)
		}

		for _, mode := range []struct{ name, keywords string }{
			{"vectors only", ""},
			{"hybrid (vectors + keywords)", vocab[10] + " " + vocab[20] + " " + vocab[30]},
		} {
			const queries = 60
			lat := make([]time.Duration, 0, queries)
			for i := 0; i < queries; i++ {
				q := randomVector(r)
				t1 := time.Now()
				if _, err := s.Search(ctx, "load", "o/big", q, mode.keywords, 6, nil); err != nil {
					t.Fatal(err)
				}
				lat = append(lat, time.Since(t1))
			}

			// Eight searches at once.
			var wg sync.WaitGroup
			var mu sync.Mutex
			par := make([]time.Duration, 0, 8*10)
			for w := 0; w < 8; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					lr := rand.New(rand.NewSource(int64(w) + 100))
					for i := 0; i < 10; i++ {
						q := randomVector(lr)
						t1 := time.Now()
						if _, err := s.Search(ctx, "load", "o/big", q, mode.keywords, 6, nil); err != nil {
							t.Error(err)
							return
						}
						mu.Lock()
						par = append(par, time.Since(t1))
						mu.Unlock()
					}
				}()
			}
			wg.Wait()

			t.Logf("chunks=%6d (indexed in %v)  %-28s one at a time: p50=%v p95=%v   8 at once: p50=%v p95=%v",
				have, insert.Round(time.Second), mode.name,
				percentileOf(lat, 0.5).Round(time.Millisecond), percentileOf(lat, 0.95).Round(time.Millisecond),
				percentileOf(par, 0.5).Round(time.Millisecond), percentileOf(par, 0.95).Round(time.Millisecond))
		}
	}
}
