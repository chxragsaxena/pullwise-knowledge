// Package chunk splits source files into pieces small enough to embed.
package chunk

import (
	"go/parser"
	"go/token"
	"strings"
)

const (
	// MaxChars is the target maximum size of a chunk.
	MaxChars = 1500
	// overlapLines are repeated at the start of the next piece of a cut block.
	overlapLines = 4
	// maxLineChars bounds one line; longer lines are truncated.
	maxLineChars = 2000
)

// Chunk is a contiguous range of a file.
type Chunk struct {
	Path string
	// StartLine and EndLine are 1-based and inclusive.
	StartLine int
	EndLine   int
	Content   string
}

// Text is what gets embedded: the path, then the code.
func (c Chunk) Text() string { return c.Path + "\n" + c.Content }

// Split divides a file into chunks. An empty or whitespace-only file yields none.
func Split(path, content string) []Chunk {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if len(l) > maxLineChars {
			lines[i] = l[:maxLineChars]
		}
	}

	if strings.HasSuffix(path, ".go") {
		if chunks, ok := splitGo(path, content, lines); ok {
			return chunks
		}
	}
	return splitRange(path, lines, 1, len(lines))
}

// span is a 1-based inclusive line range.
type span struct{ start, end int }

// splitGo cuts at top-level declarations; ok is false if the file doesn't parse.
func splitGo(path, content string, lines []string) ([]Chunk, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, content, parser.ParseComments)
	if err != nil || len(f.Decls) == 0 {
		return nil, false
	}

	// Units tile the file so comments travel with the declaration they precede.
	var units []span
	prevEnd := 0
	for _, d := range f.Decls {
		end := fset.Position(d.End()).Line
		units = append(units, span{prevEnd + 1, end})
		prevEnd = end
	}
	units[len(units)-1].end = len(lines)

	// Small neighbouring units are merged below.
	var out []Chunk
	var cur *span
	flush := func() {
		if cur != nil {
			out = append(out, makeChunk(path, lines, cur.start, cur.end))
			cur = nil
		}
	}
	for _, u := range units {
		size := charsIn(lines, u.start, u.end)
		switch {
		case size > MaxChars:
			flush()
			out = append(out, splitRange(path, lines, u.start, u.end)...)
		case cur != nil && charsIn(lines, cur.start, u.end) <= MaxChars:
			cur.end = u.end
		default:
			flush()
			c := u
			cur = &c
		}
	}
	flush()
	return out, true
}

// splitRange cuts lines[start..end] (1-based, inclusive) into pieces of at most MaxChars.
func splitRange(path string, lines []string, start, end int) []Chunk {
	var out []Chunk
	for start <= end {
		// Grow until the next line would exceed MaxChars (always take one line).
		stop, size := start, 0
		for stop <= end {
			size += len(lines[stop-1]) + 1
			if size > MaxChars && stop > start {
				break
			}
			stop++
		}
		cut := stop - 1 // last line included
		if stop <= end {
			// More follows: back up to a blank line if one sits in the second half.
			for i := cut; i > start+(cut-start)/2; i-- {
				if strings.TrimSpace(lines[i-1]) == "" {
					cut = i
					break
				}
			}
		}
		if c := makeChunk(path, lines, start, cut); strings.TrimSpace(c.Content) != "" {
			out = append(out, c)
		}
		if cut >= end {
			break
		}
		next := cut + 1 - overlapLines
		if next <= start {
			next = cut + 1 // never move backwards or stall
		}
		start = next
	}
	return out
}

func makeChunk(path string, lines []string, start, end int) Chunk {
	return Chunk{Path: path, StartLine: start, EndLine: end, Content: strings.Join(lines[start-1:end], "\n")}
}

func charsIn(lines []string, start, end int) int {
	n := 0
	for i := start; i <= end; i++ {
		n += len(lines[i-1]) + 1
	}
	return n
}
