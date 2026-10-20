package patch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/baditaflorin/fpatch/internal/spec"
)

func apply(t testing.TB, src, specText string, idempotent bool) Result {
	t.Helper()
	sp, err := spec.Parse(specText, spec.Options{})
	if err != nil {
		t.Fatalf("spec: %v\n%s", err, specText)
	}
	return Apply([]byte(src), sp.Hunks, Options{Idempotent: idempotent})
}

func statuses(r Result) []Status {
	var out []Status
	for _, h := range r.Hunks {
		out = append(out, h.Status)
	}
	return out
}

func TestApplySuccess(t *testing.T) {
	cases := []struct {
		name string
		src  string
		spec string
		want string
		st   []Status
	}{
		{
			"replace a line",
			"a\nfoo = 1\nb\n",
			"=== replace\nfoo = 1\n=== with\nfoo = 2\n=== end\n",
			"a\nfoo = 2\nb\n", []Status{Applied},
		},
		{
			"replace part of a line",
			"timeout: 30 # seconds\n",
			"=== replace\ntimeout: 30\n=== with\ntimeout: 60\n=== end\n",
			"timeout: 60 # seconds\n", []Status{Applied},
		},
		{
			"replace several lines with one",
			"def f():\n    x = 1\n    return x\n\nprint(f())\n",
			"=== replace\ndef f():\n    x = 1\n    return x\n=== with\ndef f():\n    return 1\n=== end\n",
			"def f():\n    return 1\n\nprint(f())\n", []Status{Applied},
		},
		{
			"a trailing empty line in the anchor deletes the whole line",
			"a\nfoo\nb\n",
			"=== replace\nfoo\n\n=== with\n=== end\n",
			"a\nb\n", []Status{Applied},
		},
		{
			"empty replacement deletes the text",
			"axb\n",
			"=== replace\nx\n=== with\n=== end\n",
			"ab\n", []Status{Applied},
		},
		{
			"count=2",
			"x 1\nx 2\n",
			"=== replace count=2\nx\n=== with\ny\n=== end\n",
			"y 1\ny 2\n", []Status{Applied},
		},
		{
			"count=all with three matches",
			"x x\nx\n",
			"=== replace count=all\nx\n=== with\nlonger\n=== end\n",
			"longer longer\nlonger\n", []Status{Applied},
		},
		{
			"matches do not overlap",
			"aaaa\n",
			"=== replace count=2\naa\n=== with\nb\n=== end\n",
			"bb\n", []Status{Applied},
		},
		{
			"missing final newline stays missing",
			"a\nb",
			"=== replace\nb\n=== with\nc\n=== end\n",
			"a\nc", []Status{Applied},
		},
		{
			"unicode",
			"héllo wörld\n",
			"=== replace\nwörld\n=== with\nmonde ☃\n=== end\n",
			"héllo monde ☃\n", []Status{Applied},
		},
		{
			"identical old and new is a no-op",
			"a\n",
			"=== replace\na\n=== with\na\n=== end\n",
			"a\n", []Status{NoOp},
		},
		{
			"two hunks",
			"one\ntwo\nthree\n",
			"=== replace\none\n=== with\n1\n=== end\n=== replace\nthree\n=== with\n3\n=== end\n",
			"1\ntwo\n3\n", []Status{Applied, Applied},
		},
		{
			"hunk order does not matter",
			"one\ntwo\nthree\n",
			"=== replace\nthree\n=== with\n3\n=== end\n=== replace\none\n=== with\n1\n=== end\n",
			"1\ntwo\n3\n", []Status{Applied, Applied},
		},
		{
			"insert-after in the middle",
			"[server]\nport = 1\n",
			"=== insert-after\n[server]\n=== with\nhost = x\n=== end\n",
			"[server]\nhost = x\nport = 1\n", []Status{Applied},
		},
		{
			"insert-after at the end of the file",
			"a\nb\n",
			"=== insert-after\nb\n=== with\nc\n=== end\n",
			"a\nb\nc\n", []Status{Applied},
		},
		{
			"insert-after at the end keeps a missing final newline missing",
			"a\nb",
			"=== insert-after\nb\n=== with\nc\n=== end\n",
			"a\nb\nc", []Status{Applied},
		},
		{
			"insert-after a multi-line anchor",
			"a\nb\nc\n",
			"=== insert-after\na\nb\n=== with\nX\nY\n=== end\n",
			"a\nb\nX\nY\nc\n", []Status{Applied},
		},
		{
			"insert-after an anchor that ends with a newline",
			"a\nb\n",
			"=== insert-after\na\n\n=== with\nX\n=== end\n",
			"a\nX\nb\n", []Status{Applied},
		},
		{
			"insert-after a partial-line anchor goes after the whole line",
			"x foo bar y\nnext\n",
			"=== insert-after\nfoo\n=== with\nNEW\n=== end\n",
			"x foo bar y\nNEW\nnext\n", []Status{Applied},
		},
		{
			"insert-before the first line",
			"a\nb\n",
			"=== insert-before\na\n=== with\nX\n=== end\n",
			"X\na\nb\n", []Status{Applied},
		},
		{
			"insert-before a partial-line anchor goes before the whole line",
			"x\n  foo bar\ny\n",
			"=== insert-before\nfoo\n=== with\n  # note\n=== end\n",
			"x\n  # note\n  foo bar\ny\n", []Status{Applied},
		},
		{
			"insert-before count=all",
			"k 1\nm\nk 2\n",
			"=== insert-before count=all\nk\n=== with\n--\n=== end\n",
			"--\nk 1\nm\n--\nk 2\n", []Status{Applied},
		},
		{
			"a single empty line inserts a blank line",
			"a\nb\n",
			"=== insert-after\na\n=== with\n\n=== end\n",
			"a\n\nb\n", []Status{Applied},
		},
		{
			"two inserts at one place keep spec order",
			"a\nb\n",
			"=== insert-after\na\n=== with\n1\n=== end\n=== insert-after\na\n=== with\n2\n=== end\n",
			"a\n1\n2\nb\n", []Status{Applied, Applied},
		},
		{
			"append",
			"a\n",
			"=== append\nb\n=== end\n",
			"a\nb\n", []Status{Applied},
		},
		{
			"append adds the missing newline first",
			"a",
			"=== append\nb\nc\n=== end\n",
			"a\nb\nc\n", []Status{Applied},
		},
		{
			"append to an empty file",
			"",
			"=== append\nb\n=== end\n",
			"b\n", []Status{Applied},
		},
		{
			"append a blank line",
			"a\n",
			"=== append\n\n=== end\n",
			"a\n\n", []Status{Applied},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := apply(t, tc.src, tc.spec, false)
			if r.Failed {
				t.Fatalf("Apply failed: %+v", r.Hunks)
			}
			if got := string(r.Content); got != tc.want {
				t.Errorf("content\n got: %q\nwant: %q", got, tc.want)
			}
			if got := statuses(r); !reflect.DeepEqual(got, tc.st) {
				t.Errorf("statuses = %v, want %v", got, tc.st)
			}
			if r.Changed != (tc.want != tc.src) {
				t.Errorf("Changed = %v", r.Changed)
			}
		})
	}
}

func TestApplyFailures(t *testing.T) {
	src := "alpha\nbeta\nalpha\ngamma\n"
	cases := []struct {
		name    string
		spec    string
		status  Status
		matches int
		lines   []int
	}{
		{"missing", "=== replace\ndelta\n=== with\nx\n=== end\n", Missing, 0, nil},
		{"ambiguous", "=== replace\nalpha\n=== with\nx\n=== end\n", Ambiguous, 2, []int{1, 3}},
		{"fewer matches than count", "=== replace count=3\nalpha\n=== with\nx\n=== end\n", CountMismatch, 2, []int{1, 3}},
		{"more matches than count", "=== replace count=1\nalpha\n=== with\nx\n=== end\n", Ambiguous, 2, []int{1, 3}},
		{"count=all without a match", "=== replace count=all\nzzz\n=== with\nx\n=== end\n", Missing, 0, nil},
		{"insert-after missing", "=== insert-after\nzzz\n=== with\nx\n=== end\n", Missing, 0, nil},
		{"insert-before ambiguous", "=== insert-before\nalpha\n=== with\nx\n=== end\n", Ambiguous, 2, []int{1, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := apply(t, src, tc.spec, false)
			if !r.Failed || r.Content != nil {
				t.Fatalf("Failed=%v Content=%q, want a failure with no content", r.Failed, r.Content)
			}
			h := r.Hunks[0]
			if h.Status != tc.status || h.Matches != tc.matches || !reflect.DeepEqual(h.Lines, tc.lines) {
				t.Errorf("got status=%s matches=%d lines=%v, want %s %d %v (%s)", h.Status, h.Matches, h.Lines, tc.status, tc.matches, tc.lines, h.Message)
			}
			if h.Message == "" {
				t.Error("no message")
			}
		})
	}
}

func TestOneFailingHunkFailsTheWholeFile(t *testing.T) {
	r := apply(t, "a\nb\n", "=== replace\na\n=== with\nA\n=== end\n=== replace\nzzz\n=== with\nZ\n=== end\n", false)
	if !r.Failed || r.Content != nil || r.Changed {
		t.Fatalf("want a failure without content, got Failed=%v Content=%q Changed=%v", r.Failed, r.Content, r.Changed)
	}
	if got := statuses(r); !reflect.DeepEqual(got, []Status{Applied, Missing}) {
		t.Errorf("statuses = %v", got)
	}
}

func TestHunksMatchTheOriginalFile(t *testing.T) {
	// The second anchor only exists in the result of the first hunk.
	r := apply(t, "a\n", "=== replace\na\n=== with\nb\n=== end\n=== replace\nb\n=== with\nc\n=== end\n", false)
	if !r.Failed || r.Hunks[1].Status != Missing {
		t.Fatalf("want hunk 2 to be missing, got %+v", r.Hunks)
	}
}

func TestOverlappingHunks(t *testing.T) {
	cases := []struct {
		name, src, spec string
		overlap         bool
	}{
		{"two replaces over the same text", "abcd\n",
			"=== replace\nabc\n=== with\nx\n=== end\n=== replace\nbcd\n=== with\ny\n=== end\n", true},
		{"the same replace twice", "abc\n",
			"=== replace\nabc\n=== with\nx\n=== end\n=== replace\nabc\n=== with\nx\n=== end\n", true},
		{"an insert inside a replaced region", "a\nb\nc\n",
			"=== replace\na\nb\n=== with\nx\n=== end\n=== insert-after\na\n=== with\nline\n=== end\n", true},
		{"a section over a replaced line", "# H\nbody\n",
			"=== upsert-section \"# H\"\nnew\n=== end\n=== replace\nbody\n=== with\nx\n=== end\n", true},
		{"an append after a replaced tail only touches it", "x\ntail",
			"=== replace\ntail\n=== with\nx\n=== end\n=== append\nmore\n=== end\n", false},
		{"an insert before a replaced line only touches it", "abcd\n",
			"=== replace\nabcd\n=== with\nx\n=== end\n=== insert-before\nabcd\n=== with\nline\n=== end\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := apply(t, tc.src, tc.spec, false)
			if !tc.overlap {
				if r.Failed {
					t.Fatalf("touching hunks must be fine: %+v", r.Hunks)
				}
				return
			}
			if !r.Failed || r.Content != nil {
				t.Fatalf("want an overlap failure, got %+v", r.Hunks)
			}
			found := false
			for _, h := range r.Hunks {
				if h.Status == Overlap {
					found = true
				}
			}
			if !found {
				t.Errorf("no hunk is marked as overlapping: %+v", r.Hunks)
			}
		})
	}
}

func TestIdempotent(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		spec  string
		first string // content after the first run
	}{
		{"replace", "a\nfoo\n", "=== replace\nfoo\n=== with\nbar\n=== end\n", "a\nbar\n"},
		{"replace whose new text contains the old", "timeout: 30\n", "=== replace\ntimeout: 30\n=== with\ntimeout: 30 # seconds\n=== end\n", "timeout: 30 # seconds\n"},
		{"replace count=all", "x x\n", "=== replace count=all\nx\n=== with\ny\n=== end\n", "y y\n"},
		{"deletion", "a\nfoo\nb\n", "=== replace\nfoo\n\n=== with\n=== end\n", "a\nb\n"},
		{"insert-after", "a\nb\n", "=== insert-after\na\n=== with\nX\n=== end\n", "a\nX\nb\n"},
		{"insert-after at the end", "a\nb", "=== insert-after\nb\n=== with\nX\n=== end\n", "a\nb\nX"},
		{"insert-before", "a\nb\n", "=== insert-before\nb\n=== with\nX\n=== end\n", "a\nX\nb\n"},
		{"insert-after multi-line text", "a\nb\n", "=== insert-after\na\n=== with\nX\nY\n=== end\n", "a\nX\nY\nb\n"},
		{"append", "a\n", "=== append\nb\n=== end\n", "a\nb\n"},
		{"append a blank line", "a\n", "=== append\n\n=== end\n", "a\n\n"},
		{"section", "# T\n\n## S\nold\n", "=== upsert-section \"## S\"\nnew\n=== end\n", "# T\n\n## S\nnew\n"},
		{"new section", "# T\n", "=== upsert-section \"## S\"\nnew\n=== end\n", "# T\n\n## S\nnew\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r1 := apply(t, tc.src, tc.spec, true)
			if r1.Failed || string(r1.Content) != tc.first {
				t.Fatalf("first run: failed=%v content=%q, want %q (%+v)", r1.Failed, r1.Content, tc.first, r1.Hunks)
			}
			r2 := apply(t, tc.first, tc.spec, true)
			if r2.Failed || r2.Changed || string(r2.Content) != tc.first {
				t.Fatalf("second run must change nothing: failed=%v changed=%v content=%q (%+v)", r2.Failed, r2.Changed, r2.Content, r2.Hunks)
			}
			for _, h := range r2.Hunks {
				if h.Status != AlreadyApplied {
					t.Errorf("second run status = %s, want already-applied", h.Status)
				}
			}
		})
	}
}

func TestWithoutIdempotentASecondRunIsAnError(t *testing.T) {
	spec := "=== replace\nfoo\n=== with\nbar\n=== end\n"
	if r := apply(t, "foo\n", spec, false); r.Failed {
		t.Fatal("first run failed")
	}
	r := apply(t, "bar\n", spec, false)
	if !r.Failed || r.Hunks[0].Status != Missing {
		t.Fatalf("want missing, got %+v", r.Hunks)
	}
}

func TestIdempotentStillFailsWhenNeitherOldNorNewIsThere(t *testing.T) {
	r := apply(t, "something else\n", "=== replace\nfoo\n=== with\nbar\n=== end\n", true)
	if !r.Failed || r.Hunks[0].Status != Missing {
		t.Fatalf("want missing, got %+v", r.Hunks)
	}
}

func TestIdempotentInsertOnlyAddsWhereMissing(t *testing.T) {
	src := "k 1\nX\nk 2\nk 3\n"
	r := apply(t, src, "=== insert-after count=3\nk\n=== with\nX\n=== end\n", true)
	if r.Failed {
		t.Fatalf("failed: %+v", r.Hunks)
	}
	if want := "k 1\nX\nk 2\nX\nk 3\nX\n"; string(r.Content) != want {
		t.Errorf("content = %q, want %q", r.Content, want)
	}
}

func TestLineEndingsAndBOM(t *testing.T) {
	cases := []struct {
		name, src, spec, want, ending string
	}{
		{
			"crlf replace across lines",
			"a\r\nb\r\nc\r\n",
			"=== replace\na\nb\n=== with\nx\ny\nz\n=== end\n",
			"x\r\ny\r\nz\r\nc\r\n", "crlf",
		},
		{
			"crlf insert-after",
			"a\r\nb\r\n",
			"=== insert-after\na\n=== with\nX\nY\n=== end\n",
			"a\r\nX\r\nY\r\nb\r\n", "crlf",
		},
		{
			"crlf append without a final newline",
			"a\r\nb",
			"=== append\nc\n=== end\n",
			"a\r\nb\r\nc\r\n", "crlf",
		},
		{
			"crlf section",
			"# T\r\n\r\n## S\r\nold\r\n",
			"=== upsert-section \"## S\"\nnew\n=== end\n",
			"# T\r\n\r\n## S\r\nnew\r\n", "crlf",
		},
		{
			"bom is kept",
			"\xef\xbb\xbfa\nb\n",
			"=== replace\na\n=== with\nA\n=== end\n",
			"\xef\xbb\xbfA\nb\n", "lf",
		},
		{
			"bom and crlf",
			"\xef\xbb\xbfa\r\nb\r\n",
			"=== insert-before\na\n=== with\nX\n=== end\n",
			"\xef\xbb\xbfX\r\na\r\nb\r\n", "crlf",
		},
		{
			"mixed endings are matched byte for byte",
			"a\r\nb\n",
			"=== replace\na\n=== with\nx\n=== end\n",
			"x\r\nb\n", "mixed",
		},
		{
			"a lone CR survives",
			"a\rb\n",
			"=== replace\na\n=== with\nx\n=== end\n",
			"x\rb\n", "lf",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := apply(t, tc.src, tc.spec, false)
			if r.Failed {
				t.Fatalf("failed: %+v", r.Hunks)
			}
			if string(r.Content) != tc.want {
				t.Errorf("content\n got: %q\nwant: %q", r.Content, tc.want)
			}
			if r.LineEnding != tc.ending {
				t.Errorf("LineEnding = %s, want %s", r.LineEnding, tc.ending)
			}
		})
	}
}

func TestMixedEndingsExplainAMissedMultiLineAnchor(t *testing.T) {
	r := apply(t, "a\r\nb\nc\n", "=== replace\na\nb\n=== with\nx\n=== end\n", false)
	if !r.Failed {
		t.Fatal("a multi-line anchor over a CRLF/LF seam must not match")
	}
	if got := strings.Join(r.Hunks[0].Hints, "|"); !strings.Contains(got, "mixes LF and CRLF") {
		t.Errorf("hints = %q", got)
	}
}

func TestDiffViewsAreNormalised(t *testing.T) {
	r := apply(t, "\xef\xbb\xbfa\r\nb\r\n", "=== replace\na\n=== with\nA\n=== end\n", false)
	if r.Before != "a\nb\n" || r.After != "A\nb\n" {
		t.Errorf("Before=%q After=%q", r.Before, r.After)
	}
}

const doc = "# Title\n\nintro\n\n## Install\n\nold install\n\n### Detail\n\nsub\n\n## Usage\n\nuse it\n"

func TestSections(t *testing.T) {
	cases := []struct {
		name string
		src  string
		spec string
		want string
		st   Status
	}{
		{
			"replace a section including its subsections",
			doc,
			"=== upsert-section \"## Install\"\n\nnew install\n=== end\n",
			"# Title\n\nintro\n\n## Install\n\nnew install\n\n## Usage\n\nuse it\n", Applied,
		},
		{
			"replace the last section",
			doc,
			"=== upsert-section \"## Usage\"\n\nbetter\n=== end\n",
			"# Title\n\nintro\n\n## Install\n\nold install\n\n### Detail\n\nsub\n\n## Usage\n\nbetter\n", Applied,
		},
		{
			"replace a subsection stops at the next heading of the same or a higher level",
			doc,
			"=== upsert-section \"### Detail\"\n\nnewer\n=== end\n",
			"# Title\n\nintro\n\n## Install\n\nold install\n\n### Detail\n\nnewer\n\n## Usage\n\nuse it\n", Applied,
		},
		{
			"replace the last section of a file without a final newline",
			"# T\n\n## S\nold",
			"=== upsert-section \"## S\"\nnew\n=== end\n",
			"# T\n\n## S\nnew", Applied,
		},
		{
			"the body is taken as written, trailing blank lines are the file's own",
			"## S\nold\n\n\n## T\n",
			"=== upsert-section \"## S\"\nnew\n\n\n\n=== end\n",
			"## S\nnew\n\n\n## T\n", Applied,
		},
		{
			"an empty body leaves just the heading",
			"## S\nold\n\n## T\n",
			"=== upsert-section \"## S\"\n=== end\n",
			"## S\n\n## T\n", Applied,
		},
		{
			"add a missing section after a blank line",
			"# Title\n\ntext\n",
			"=== upsert-section \"## License\"\n\nMIT.\n=== end\n",
			"# Title\n\ntext\n\n## License\n\nMIT.\n", Applied,
		},
		{
			"add a missing section when the file ends with a blank line",
			"# Title\n\n",
			"=== upsert-section \"## License\"\nMIT.\n=== end\n",
			"# Title\n\n## License\nMIT.\n", Applied,
		},
		{
			"add a missing section when the final newline is missing",
			"# Title\ntext",
			"=== upsert-section \"## License\"\nMIT.\n=== end\n",
			"# Title\ntext\n\n## License\nMIT.\n", Applied,
		},
		{
			"add a section to an empty file",
			"",
			"=== upsert-section \"# Notes\"\nhello\n=== end\n",
			"# Notes\nhello\n", Applied,
		},
		{
			"a heading inside a fenced code block is not a heading",
			"# T\n\n```\n## Install\nfake\n```\n\n## Install\n\nreal\n",
			"=== upsert-section \"## Install\"\n\nnew\n=== end\n",
			"# T\n\n```\n## Install\nfake\n```\n\n## Install\n\nnew\n", Applied,
		},
		{
			"a tilde fence is closed by a tilde fence",
			"~~~\n```\n## Install\n~~~\n\n## Install\nreal\n",
			"=== upsert-section \"## Install\"\nnew\n=== end\n",
			"~~~\n```\n## Install\n~~~\n\n## Install\nnew\n", Applied,
		},
		{
			"closing hashes are not part of the heading",
			"## Install ##\nold\n",
			"=== upsert-section \"## Install\"\nnew\n=== end\n",
			"## Install ##\nnew\n", Applied,
		},
		{
			"another level is another heading",
			"### Install\nkeep\n",
			"=== upsert-section \"## Install\"\nnew\n=== end\n",
			"### Install\nkeep\n\n## Install\nnew\n", Applied,
		},
		{
			"identical content changes nothing",
			"## S\nbody\n\n## T\n",
			"=== upsert-section \"## S\"\nbody\n=== end\n",
			"## S\nbody\n\n## T\n", AlreadyApplied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := apply(t, tc.src, tc.spec, false)
			if r.Failed {
				t.Fatalf("failed: %+v", r.Hunks)
			}
			if string(r.Content) != tc.want {
				t.Errorf("content\n got: %q\nwant: %q", r.Content, tc.want)
			}
			if r.Hunks[0].Status != tc.st {
				t.Errorf("status = %s, want %s", r.Hunks[0].Status, tc.st)
			}
		})
	}
}

func TestSectionAmbiguous(t *testing.T) {
	src := "## S\na\n\n## T\n\n## S\nb\n"
	r := apply(t, src, "=== upsert-section \"## S\"\nx\n=== end\n", false)
	if !r.Failed || r.Content != nil {
		t.Fatal("two sections with the same heading must fail")
	}
	h := r.Hunks[0]
	if h.Status != Ambiguous || h.Matches != 2 || !reflect.DeepEqual(h.Lines, []int{1, 6}) {
		t.Errorf("got %s matches=%d lines=%v", h.Status, h.Matches, h.Lines)
	}
}

func TestNearestMatch(t *testing.T) {
	file := "server:\n  host: example.org\n  port: 8080\n  timeout: 30\nlogging:\n  level: info\n"
	n := nearest(file, "  host: example.org\n  port: 8081\n  timeout: 30")
	if n == nil {
		t.Fatal("no nearest match")
	}
	if n.StartLine != 2 || n.EndLine != 4 {
		t.Errorf("nearest lines %d-%d, want 2-4", n.StartLine, n.EndLine)
	}
	if n.Similarity < 0.8 || n.Similarity >= 1 {
		t.Errorf("similarity = %v", n.Similarity)
	}
	if len(n.Lines) != 3 || n.Lines[1] != "  port: 8080" {
		t.Errorf("lines = %q", n.Lines)
	}

	if n := nearest(file, "completely unrelated zzzz qqqq"); n != nil {
		t.Errorf("unrelated anchor got a nearest match: %+v", n)
	}
	if n := nearest(file, "\n\n"); n != nil {
		t.Errorf("a blank anchor got a nearest match: %+v", n)
	}
	if n := nearest("", "x"); n != nil {
		t.Errorf("an empty file got a nearest match: %+v", n)
	}
	// An anchor with more lines than the file is compared against the whole file.
	if n := nearest("a\nb\n", "a\nb\nc\nd"); n == nil || n.StartLine != 1 || n.EndLine != 2 {
		t.Errorf("long anchor: %+v", n)
	}
	// Indentation differences still point at the right place.
	n = nearest("if x:\n\treturn 1\n", "if x:\n    return 1")
	if n == nil || n.StartLine != 1 || n.Similarity != 1 {
		t.Errorf("whitespace-only difference: %+v", n)
	}
}

func TestMissReportsNearestAndHints(t *testing.T) {
	src := "def f():\n\treturn 1\n\nprint(f())\n"
	r := apply(t, src, "=== replace\ndef f():\n    return 1\n=== with\ndef f():\n    return 2\n=== end\n", false)
	h := r.Hunks[0]
	if h.Status != Missing || h.Nearest == nil {
		t.Fatalf("want a miss with a nearest match, got %+v", h)
	}
	if h.Nearest.StartLine != 1 || h.Nearest.EndLine != 2 {
		t.Errorf("nearest = %d-%d, want 1-2", h.Nearest.StartLine, h.Nearest.EndLine)
	}
	joined := strings.Join(h.Hints, "|")
	if !strings.Contains(joined, "whitespace") {
		t.Errorf("hints = %q, want the whitespace hint", joined)
	}

	r = apply(t, "Hello World\n", "=== replace\nhello world\n=== with\nx\n=== end\n", false)
	if joined := strings.Join(r.Hunks[0].Hints, "|"); !strings.Contains(joined, "upper and lower case") {
		t.Errorf("hints = %q, want the case hint", joined)
	}
}

func TestNearestSkipsHugeSearches(t *testing.T) {
	big := strings.Repeat("some line of text\n", 200000)
	anchor := strings.Repeat("other line of text\n", 100)
	if n := nearest(big, anchor); n != nil {
		t.Error("a search above the work limit must be skipped")
	}
}

// FuzzReplace checks Apply against strings.Replace for the plain case, and
// that Apply never panics whatever the input.
func FuzzReplace(f *testing.F) {
	f.Add("a\nb\nc\n", "b", "B")
	f.Add("", "x", "y")
	f.Add("aaa", "aa", "b")
	f.Add("x\r\ny\r\n", "x\ny", "z")
	f.Fuzz(func(t *testing.T, src, old, nw string) {
		if old == "" || strings.ContainsAny(src+old+nw, "\r\x00") || strings.HasPrefix(src, bom) {
			return
		}
		h := spec.Hunk{Op: spec.OpReplace, Index: 1, Line: 1, Count: 1, Old: strings.Split(old, "\n"), New: strings.Split(nw, "\n")}
		if nw == "" {
			h.New = nil
		}
		r := Apply([]byte(src), []spec.Hunk{h}, Options{})
		n := strings.Count(src, old)
		switch {
		case n == 1:
			if r.Failed || string(r.Content) != strings.Replace(src, old, nw, 1) {
				t.Fatalf("single match: failed=%v content=%q want %q", r.Failed, r.Content, strings.Replace(src, old, nw, 1))
			}
		default:
			// strings.Count counts non-overlapping matches, like findAll.
			if !r.Failed || r.Content != nil {
				t.Fatalf("%d matches must fail: %+v", n, r.Hunks)
			}
		}
	})
}
