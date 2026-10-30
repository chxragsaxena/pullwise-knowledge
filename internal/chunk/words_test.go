package chunk

import (
	"fmt"
	"strings"
	"testing"
)

func TestWordsSplitIdentifiersAndKeepTheWhole(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"CohereEmbedder", "cohereembedder cohere embedder"},
		{"embed_v5_pro", "embedv5pro embed v 5 pro"},
		{"HTTPServer", "httpserver http server"},
		{"internal/embed/cohere.go", "internal embed cohere go"},
		{"retry", "retry"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := strings.Join(Words(tt.in), " "); got != tt.want {
			t.Errorf("Words(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestQueryTermsDropFillerShortAndRepeatedWords(t *testing.T) {
	got := QueryTerms("Which LLM does it use? The embedding model, the Embedding API")
	want := "llm embedding model api"
	if s := strings.Join(got, " "); s != want {
		t.Fatalf("QueryTerms = %q, want %q", s, want)
	}
	if got := QueryTerms("how does it work"); len(got) != 1 || got[0] != "work" {
		t.Fatalf("QueryTerms = %q", got)
	}
}

func TestSearchTextIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "word%d ", i)
	}
	if n := len(strings.Fields(SearchText(b.String()))); n != maxSearchWords {
		t.Fatalf("SearchText kept %d words, want %d", n, maxSearchWords)
	}
}
