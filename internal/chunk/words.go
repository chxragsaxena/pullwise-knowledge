package chunk

import (
	"strings"
	"unicode"
)

const (
	minTermLen = 3
	// maxSearchWords bounds the text stored per chunk for keyword search.
	maxSearchWords = 600
)

// stopWords are common English words that say nothing about code.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true, "that": true, "are": true,
	"was": true, "were": true, "from": true, "into": true, "what": true, "which": true, "who": true,
	"how": true, "does": true, "did": true, "can": true, "could": true, "should": true, "would": true,
	"where": true, "when": true, "why": true, "there": true, "their": true, "they": true, "its": true,
	"use": true, "used": true, "uses": true, "using": true, "any": true, "all": true, "not": true,
	"has": true, "have": true, "had": true, "you": true, "your": true, "about": true, "than": true,
	"then": true, "also": true, "but": true, "out": true, "get": true, "tell": true, "show": true,
}

// Words splits text into lowercase search words. Identifiers are kept whole and also split at
// camelCase, snake_case and digit boundaries, so "CohereEmbedder" yields cohereembedder, cohere, embedder.
func Words(text string) []string {
	var out []string
	for _, run := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		whole := strings.ToLower(strings.ReplaceAll(run, "_", ""))
		parts := splitIdentifier(run)
		if len(parts) > 1 && len(whole) >= minTermLen {
			out = append(out, whole)
		}
		out = append(out, parts...)
	}
	return out
}

// splitIdentifier cuts one identifier at underscores and at lower-to-upper or letter/digit changes.
func splitIdentifier(id string) []string {
	var parts []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(id)
	for i, r := range runes {
		switch {
		case r == '_':
			flush()
			continue
		case i > 0 && len(cur) > 0:
			prev := runes[i-1]
			upperAfterLower := unicode.IsUpper(r) && unicode.IsLower(prev)
			// "HTTPServer": a capital followed by a lowercase letter starts a new word.
			acronymEnd := unicode.IsUpper(r) && unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
			digitChange := unicode.IsDigit(r) != unicode.IsDigit(prev)
			if upperAfterLower || acronymEnd || digitChange {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return parts
}

// SearchText is the keyword-search form of text: its words, space separated and capped in length.
func SearchText(text string) string {
	words := Words(text)
	if len(words) > maxSearchWords {
		words = words[:maxSearchWords]
	}
	return strings.Join(words, " ")
}

// QueryTerms returns the distinct, meaningful words of a keyword query.
func QueryTerms(query string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range Words(query) {
		if len(w) < minTermLen || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}
