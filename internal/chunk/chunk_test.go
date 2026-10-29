package chunk

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitEmptyAndBlank(t *testing.T) {
	for _, in := range []string{"", "   \n\t\n"} {
		if got := Split("a.go", in); len(got) != 0 {
			t.Errorf("Split(%q) = %v, want nothing", in, got)
		}
	}
}

func TestSmallGoFileIsOneChunkWithRightLines(t *testing.T) {
	src := "package x\n\nimport \"fmt\"\n\nfunc A() { fmt.Println(1) }\n\nfunc B() { fmt.Println(2) }\n"
	got := Split("x.go", src)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1: %+v", len(got), got)
	}
	if got[0].StartLine != 1 || got[0].EndLine != 8 || got[0].Path != "x.go" {
		t.Fatalf("chunk = %+v", got[0])
	}
}

// bigFunc returns a function whose body is about size characters.
func bigFunc(name string, size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "func %s() {\n", name)
	for b.Len() < size {
		b.WriteString("\t_ = \"padding padding padding\"\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func TestGoDeclarationsAreNotCutInTheMiddle(t *testing.T) {
	src := "package x\n\n" + bigFunc("First", 1200) + "\n" + bigFunc("Second", 1200) + "\n" + bigFunc("Third", 1200)
	got := Split("x.go", src)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want one per function: %+v", len(got), got)
	}
	for i, name := range []string{"First", "Second", "Third"} {
		c := got[i]
		if !strings.Contains(c.Content, "func "+name+"()") {
			t.Errorf("chunk %d should contain %s: %q", i, name, c.Content[:40])
		}
		if strings.Count(c.Content, "func ") != 1 {
			t.Errorf("chunk %d mixes functions", i)
		}
		if strings.TrimSpace(c.Content)[len(strings.TrimSpace(c.Content))-1] != '}' {
			t.Errorf("chunk %d does not end at the closing brace", i)
		}
	}
}

func TestOversizedDeclarationIsSplitWithOverlap(t *testing.T) {
	src := "package x\n\n" + bigFunc("Huge", 6000)
	got := Split("x.go", src)
	if len(got) < 3 {
		t.Fatalf("got %d chunks, want the huge function split into several", len(got))
	}
	for i, c := range got {
		if len(c.Content) > MaxChars+maxLineChars {
			t.Errorf("chunk %d is %d chars, over the limit", i, len(c.Content))
		}
		if i > 0 {
			if c.StartLine > got[i-1].EndLine {
				t.Errorf("chunk %d starts at %d but the previous ended at %d: lines were lost", i, c.StartLine, got[i-1].EndLine)
			}
			if c.StartLine <= got[i-1].StartLine {
				t.Errorf("chunk %d does not advance", i)
			}
		}
	}
	last := got[len(got)-1]
	total := strings.Count(src, "\n") + 1
	if last.EndLine < total-1 {
		t.Errorf("the last chunk ends at %d, the file has %d lines: the tail was dropped", last.EndLine, total)
	}
}

func TestGoFileThatDoesNotParseFallsBackToLines(t *testing.T) {
	got := Split("broken.go", "package x\nfunc (((( {\n")
	if len(got) != 1 || !strings.Contains(got[0].Content, "((((") {
		t.Fatalf("got %+v, want the broken file chunked by lines rather than lost", got)
	}
}

func TestNonGoFilesSplitByLinesPreferringBlankLines(t *testing.T) {
	var b strings.Builder
	for block := 0; block < 6; block++ {
		for i := 0; i < 9; i++ {
			fmt.Fprintf(&b, "line %d of block %d with some padding text here\n", i, block)
		}
		b.WriteString("\n") // blank line between blocks
	}
	got := Split("notes.py", b.String())
	if len(got) < 2 {
		t.Fatalf("got %d chunks, want several", len(got))
	}
	for i, c := range got[:len(got)-1] {
		last := strings.Split(c.Content, "\n")
		if strings.TrimSpace(last[len(last)-1]) != "" {
			t.Errorf("chunk %d was cut mid-block instead of at a blank line: ends %q", i, last[len(last)-1])
		}
	}
}

func TestVeryLongLinesAreTruncatedAndLineEndingsNormalised(t *testing.T) {
	got := Split("min.js", strings.Repeat("x", 50_000)+"\r\nsecond\r\n")
	if len(got) == 0 {
		t.Fatal("expected chunks")
	}
	for _, c := range got {
		if len(c.Content) > MaxChars+maxLineChars {
			t.Fatalf("chunk of %d chars from a minified line", len(c.Content))
		}
		if strings.Contains(c.Content, "\r") {
			t.Fatal("carriage returns were kept")
		}
	}
}

func TestEveryLineIsCoveredAndNoChunkIsEmpty(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "row %d: some reasonably long content to force splitting\n", i)
	}
	got := Split("data.txt", b.String())
	covered := map[int]bool{}
	for _, c := range got {
		if strings.TrimSpace(c.Content) == "" {
			t.Error("empty chunk")
		}
		for l := c.StartLine; l <= c.EndLine; l++ {
			covered[l] = true
		}
	}
	for l := 1; l <= 400; l++ {
		if !covered[l] {
			t.Fatalf("line %d is in no chunk", l)
		}
	}
}

func TestTextPutsThePathFirst(t *testing.T) {
	c := Chunk{Path: "pkg/a.go", Content: "func A() {}"}
	if got := c.Text(); got != "pkg/a.go\nfunc A() {}" {
		t.Fatalf("Text() = %q", got)
	}
}
