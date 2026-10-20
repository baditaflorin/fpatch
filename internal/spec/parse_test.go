package spec

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, text string, opt Options) *Spec {
	t.Helper()
	s, err := Parse(text, opt)
	if err != nil {
		t.Fatalf("Parse: %v\n--- spec ---\n%s", err, text)
	}
	return s
}

func TestParseHunkTypes(t *testing.T) {
	text := `# a comment before the first block

=== replace
timeout: 30
retries: 3
=== with
timeout: 60
=== end

=== replace count=all
foo
=== with
=== end

=== insert-after count=2
[server]
=== with
port = 8080

=== end
=== insert-before
[logging]
=== with
# logging follows
=== end
=== append
last line
=== end
=== upsert-section "## Install"
Run go install.
=== end
=== upsert-section ## Usage
=== end
`
	s := mustParse(t, text, Options{})
	if s.Scoped {
		t.Fatal("spec without file directives must not be scoped")
	}
	want := []Hunk{
		{Op: OpReplace, Index: 1, Line: 3, Count: 1, Old: []string{"timeout: 30", "retries: 3"}, New: []string{"timeout: 60"}},
		{Op: OpReplace, Index: 2, Line: 10, Count: 1, All: true, Old: []string{"foo"}},
		{Op: OpInsertAfter, Index: 3, Line: 15, Count: 2, Old: []string{"[server]"}, New: []string{"port = 8080", ""}},
		{Op: OpInsertBefore, Index: 4, Line: 21, Count: 1, Old: []string{"[logging]"}, New: []string{"# logging follows"}},
		{Op: OpAppend, Index: 5, Line: 26, Count: 1, New: []string{"last line"}},
		{Op: OpUpsertSection, Index: 6, Line: 29, Count: 1, Heading: "## Install", New: []string{"Run go install."}},
		{Op: OpUpsertSection, Index: 7, Line: 32, Count: 1, Heading: "## Usage"},
	}
	if !reflect.DeepEqual(s.Hunks, want) {
		t.Fatalf("hunks differ\n got: %#v\nwant: %#v", s.Hunks, want)
	}
	if got := s.Hunks[0].OldText(); got != "timeout: 30\nretries: 3" {
		t.Errorf("OldText = %q", got)
	}
	if got := s.Hunks[2].NewText(); got != "port = 8080\n" {
		t.Errorf("NewText = %q", got)
	}
}

func TestParseScopes(t *testing.T) {
	text := `=== file a.txt
=== replace
x
=== with
y
=== end
=== append
z
=== end
=== file dir/b.txt
=== append
w
=== end
`
	s := mustParse(t, text, Options{})
	if !s.Scoped {
		t.Fatal("expected a scoped spec")
	}
	if got := s.Files(); !reflect.DeepEqual(got, []string{"a.txt", "dir/b.txt"}) {
		t.Errorf("Files = %v", got)
	}
	if got := len(s.HunksFor("a.txt")); got != 2 {
		t.Errorf("a.txt hunks = %d, want 2", got)
	}
	if got := len(s.HunksFor("dir/b.txt")); got != 1 {
		t.Errorf("dir/b.txt hunks = %d, want 1", got)
	}
	if got := len(s.HunksFor("other")); got != 0 {
		t.Errorf("other hunks = %d, want 0", got)
	}
	if got := s.HunksFor("a.txt")[1].Index; got != 2 {
		t.Errorf("hunk index is global: got %d, want 2", got)
	}
}

func TestUnscopedHunksApplyToEveryFile(t *testing.T) {
	s := mustParse(t, "=== append\nx\n=== end\n", Options{})
	if len(s.HunksFor("anything")) != 1 || len(s.HunksFor("else")) != 1 {
		t.Fatal("an unscoped hunk must apply to every file")
	}
}

func TestParseLineEndings(t *testing.T) {
	lf := mustParse(t, "=== replace\na\nb\n=== with\nc\n=== end\n", Options{})
	crlf := mustParse(t, "=== replace\r\na\r\nb\r\n=== with\r\nc\r\n=== end\r\n", Options{})
	noFinal := mustParse(t, "=== replace\na\nb\n=== with\nc\n=== end", Options{})
	bom := mustParse(t, "\ufeff=== replace\na\nb\n=== with\nc\n=== end\n", Options{})
	for name, s := range map[string]*Spec{"crlf": crlf, "no final newline": noFinal, "bom": bom} {
		if !reflect.DeepEqual(s.Hunks, lf.Hunks) {
			t.Errorf("%s: hunks differ from the LF spec: %#v vs %#v", name, s.Hunks, lf.Hunks)
		}
	}
}

func TestParseTrailingSpaceOnDirectives(t *testing.T) {
	s := mustParse(t, "=== replace  \na\n=== with \t\nb\n=== end   \n", Options{})
	if len(s.Hunks) != 1 || s.Hunks[0].OldText() != "a" || s.Hunks[0].NewText() != "b" {
		t.Fatalf("unexpected hunks: %#v", s.Hunks)
	}
}

func TestContentLinesKeepTheirWhitespaceAndLookalikes(t *testing.T) {
	text := "=== replace\n  indented\n\ttab\ntrailing  \n===not a directive\n==== also not\n=== foo bar\n=== with\n=== end\n"
	s := mustParse(t, text, Options{})
	want := []string{"  indented", "\ttab", "trailing  ", "===not a directive", "==== also not", "=== foo bar"}
	if !reflect.DeepEqual(s.Hunks[0].Old, want) {
		t.Fatalf("Old = %q, want %q", s.Hunks[0].Old, want)
	}
}

func TestCustomMarker(t *testing.T) {
	text := "@@@ replace\n=== end\n@@@ with\n=== with\n@@@ end\n"
	s := mustParse(t, text, Options{Marker: "@@@"})
	if s.Hunks[0].OldText() != "=== end" || s.Hunks[0].NewText() != "=== with" {
		t.Fatalf("unexpected hunk: %#v", s.Hunks[0])
	}
	// A marker that also starts ordinary text only claims real directives.
	text = "### replace\n### Heading\n### with\n## Other\n### end\n"
	s = mustParse(t, text, Options{Marker: "###"})
	if s.Hunks[0].OldText() != "### Heading" || s.Hunks[0].NewText() != "## Other" {
		t.Fatalf("unexpected hunk: %#v", s.Hunks[0])
	}
}

func TestParseHeading(t *testing.T) {
	good := map[string][2]any{
		"# Title":          {1, "Title"},
		"## Install":       {2, "Install"},
		"  ###\tTabbed  ":  {3, "Tabbed"},
		"###### Six":       {6, "Six"},
		"## With ## marks": {2, "With ## marks"},
	}
	for in, want := range good {
		level, text, ok := ParseHeading(in)
		if !ok || level != want[0].(int) || text != want[1].(string) {
			t.Errorf("ParseHeading(%q) = %d, %q, %v; want %v", in, level, text, ok, want)
		}
	}
	for _, in := range []string{"", "#", "##", "Install", "#Install", "####### Seven", "## ", "-- x"} {
		if _, _, ok := ParseHeading(in); ok {
			t.Errorf("ParseHeading(%q) accepted", in)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		text string
		line int // 0: whole spec
		msg  string
	}{
		{"empty", "", 0, "no hunks"},
		{"only comments", "# nothing\n\n", 0, "no hunks"},
		{"text outside block", "hello\n", 1, "unexpected text outside a block"},
		{"unknown directive", "=== frobnicate\n", 1, "unknown directive"},
		{"with outside", "=== with\n", 1, "outside a block"},
		{"end outside", "=== end\n", 1, "outside a block"},
		{"unterminated replace", "=== replace\na\n=== with\nb\n", 1, "never closed"},
		{"unterminated append", "=== append\nx\n", 1, "never closed"},
		{"missing with", "=== replace\na\n=== end\n", 3, `needs "=== with"`},
		{"second with", "=== replace\na\n=== with\nb\n=== with\n=== end\n", 5, "unexpected"},
		{"with in append", "=== append\n=== with\n=== end\n", 2, "unexpected"},
		{"forgotten end", "=== replace\na\n=== with\nb\n=== replace\nc\n=== with\nd\n=== end\n", 5, "unexpected"},
		{"empty anchor", "=== replace\n=== with\nx\n=== end\n", 1, "empty"},
		{"insert without text", "=== insert-after\nanchor\n=== with\n=== end\n", 1, "nothing to insert"},
		{"insert without anchor", "=== insert-before\n=== with\nx\n=== end\n", 1, "anchor text is empty"},
		{"append without text", "=== append\n=== end\n", 1, "nothing to append"},
		{"bad option", "=== replace foo=1\na\n=== with\n=== end\n", 1, "unknown option"},
		{"bad count word", "=== replace count=many\na\n=== with\n=== end\n", 1, "bad count"},
		{"zero count", "=== replace count=0\na\n=== with\n=== end\n", 1, "bad count"},
		{"negative count", "=== insert-after count=-2\na\n=== with\nb\n=== end\n", 1, "bad count"},
		{"append option", "=== append count=2\nx\n=== end\n", 1, "takes no options"},
		{"heading not atx", "=== upsert-section Install\n=== end\n", 1, "Markdown heading"},
		{"heading unterminated quote", "=== upsert-section \"## x\n=== end\n", 1, "bad quoted heading"},
		{"file without path", "=== file\n=== append\nx\n=== end\n", 1, "needs a file path"},
		{"file without hunks", "=== file a\n=== file b\n=== append\nx\n=== end\n", 1, "no hunks"},
		{"trailing file", "=== file a\n=== append\nx\n=== end\n=== file b\n", 5, "no hunks"},
		{"mixed scope", "=== append\nx\n=== end\n=== file a\n=== append\ny\n=== end\n", 4, "put every hunk under"},
		{"invalid utf8", "=== append\n\xff\n=== end\n", 0, "UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.text, Options{})
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("Parse error = %v, want a *SyntaxError", err)
			}
			if se.Line != tc.line {
				t.Errorf("error line = %d, want %d (%v)", se.Line, tc.line, err)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error %q does not contain %q", err, tc.msg)
			}
		})
	}
}

func TestParseBadMarker(t *testing.T) {
	for _, m := range []string{"a b", "x\n", "\t"} {
		if _, err := Parse("x", Options{Marker: m}); err == nil {
			t.Errorf("marker %q accepted", m)
		}
	}
}

func stripLines(s *Spec) *Spec {
	c := *s
	c.Hunks = append([]Hunk(nil), s.Hunks...)
	for i := range c.Hunks {
		c.Hunks[i].Line = 0
	}
	return &c
}

func TestFormatRoundTrip(t *testing.T) {
	text := "=== file a\n=== replace count=3\nx\n  y\n=== with\n\nz\n=== end\n=== upsert-section \"## A \\\"q\\\"\"\nbody\n=== end\n=== file b\n=== insert-before count=all\n= = =\n=== with\nl1\n\n=== end\n=== append\n=== foo\n=== end\n"
	s := mustParse(t, text, Options{})
	again := mustParse(t, Format(s), Options{})
	if !reflect.DeepEqual(stripLines(s), stripLines(again)) {
		t.Fatalf("round trip differs\n first: %#v\nsecond: %#v\ntext:\n%s", s, again, Format(s))
	}
}

// FuzzParse checks that Parse never panics, and that whatever it accepts
// survives Format and Parse again unchanged.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"=== replace\na\n=== with\nb\n=== end\n",
		"=== replace count=all\na\n=== with\n=== end\n",
		"=== file x\n=== append\nz\n=== end\n",
		"=== upsert-section \"## Hi\"\nbody\n=== end",
		"=== insert-after count=2\nanchor\n=== with\n\n=== end\n",
		"# c\n=== append\r\nx\r\n=== end\r\n",
		"=== replace\n=== end\n",
		"=== frob\n",
		"\ufeff=== append\nx\n=== end\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		s, err := Parse(text, Options{})
		if err != nil {
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("error is not a *SyntaxError: %v", err)
			}
			return
		}
		if len(s.Hunks) == 0 {
			t.Fatal("Parse succeeded with no hunks")
		}
		for i, h := range s.Hunks {
			if h.Index != i+1 {
				t.Fatalf("hunk %d has Index %d", i+1, h.Index)
			}
			if s.Scoped != (h.File != "") {
				t.Fatalf("hunk %d: Scoped=%v but File=%q", i+1, s.Scoped, h.File)
			}
		}
		again, err := Parse(Format(s), Options{})
		if err != nil {
			t.Fatalf("Format output does not parse: %v\n%s", err, Format(s))
		}
		if !reflect.DeepEqual(stripLines(s), stripLines(again)) {
			t.Fatalf("round trip differs\n first: %#v\nsecond: %#v", s, again)
		}
	})
}
