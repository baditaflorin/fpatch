package diff

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@\n$`)

// applyUnified applies the unified diff d to a, checking context and removed
// lines along the way. It is a deliberately small, independent reader of the
// format so the tests do not trust the writer.
func applyUnified(t *testing.T, a, d string) string {
	t.Helper()
	if d == "" {
		return a
	}
	al := splitLines(a)
	dl := strings.SplitAfter(d, "\n")
	if len(dl) < 3 || !strings.HasPrefix(dl[0], "--- ") || !strings.HasPrefix(dl[1], "+++ ") {
		t.Fatalf("bad diff header:\n%s", d)
	}
	var out []string
	pos, i := 0, 2
	for i < len(dl) && dl[i] != "" {
		m := hunkHeader.FindStringSubmatch(dl[i])
		if m == nil {
			t.Fatalf("bad hunk header %q in:\n%s", dl[i], d)
		}
		aStart, aCount := atoi(m[1]), 1
		if m[2] != "" {
			aCount = atoi(m[2])
		}
		start := aStart - 1
		if aCount == 0 {
			start = aStart
		}
		if start < pos || start > len(al) {
			t.Fatalf("hunk starts at %d, already at %d of %d lines:\n%s", start, pos, len(al), d)
		}
		out = append(out, al[pos:start]...)
		pos = start
		i++
		for i < len(dl) && dl[i] != "" && !strings.HasPrefix(dl[i], "@@") {
			line := dl[i]
			content := line[1:]
			noNL := i+1 < len(dl) && strings.HasPrefix(dl[i+1], `\ No newline`)
			if noNL {
				content = strings.TrimSuffix(content, "\n")
			}
			switch line[0] {
			case ' ', '-':
				if pos >= len(al) || al[pos] != content {
					t.Fatalf("diff line %q does not match the source at line %d:\n%s", line, pos+1, d)
				}
				if line[0] == ' ' {
					out = append(out, al[pos])
				}
				pos++
			case '+':
				out = append(out, content)
			default:
				t.Fatalf("unexpected diff line %q", line)
			}
			i++
			if noNL {
				i++
			}
		}
	}
	out = append(out, al[pos:]...)
	return strings.Join(out, "")
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestUnifiedExact(t *testing.T) {
	cases := []struct {
		name, a, b, want string
	}{
		{"equal", "a\nb\n", "a\nb\n", ""},
		{"both empty", "", "", ""},
		{
			"replace one line",
			"a\nb\nc\n", "a\nB\nc\n",
			"--- f\n+++ f\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n",
		},
		{
			"add to empty",
			"", "x\n",
			"--- f\n+++ f\n@@ -0,0 +1 @@\n+x\n",
		},
		{
			"delete everything",
			"x\ny\n", "",
			"--- f\n+++ f\n@@ -1,2 +0,0 @@\n-x\n-y\n",
		},
		{
			"missing final newline gained",
			"a\nb", "a\nb\n",
			"--- f\n+++ f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n",
		},
		{
			"missing final newline kept in context",
			"a\nb\nc", "a\nB\nc",
			"--- f\n+++ f\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n\\ No newline at end of file\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Unified("f", tc.a, tc.b)
			if got != tc.want {
				t.Errorf("diff mismatch\n got:\n%s\nwant:\n%s", got, tc.want)
			}
			if applied := applyUnified(t, tc.a, got); applied != tc.b {
				t.Errorf("applying the diff gave %q, want %q", applied, tc.b)
			}
		})
	}
}

func TestUnifiedContextAndHunkSplitting(t *testing.T) {
	var a, b []string
	for i := 1; i <= 40; i++ {
		a = append(a, "line "+string(rune('a'+i%26))+"\n")
	}
	b = append(b, a...)
	b[2] = "changed 3\n"
	b[30] = "changed 31\n"
	d := Unified("f", strings.Join(a, ""), strings.Join(b, ""))
	if got := strings.Count(d, "@@ -"); got != 2 {
		t.Errorf("want 2 hunks, got %d:\n%s", got, d)
	}
	if !strings.Contains(d, "@@ -1,6 +1,6 @@") || !strings.Contains(d, "@@ -28,7 +28,7 @@") {
		t.Errorf("unexpected hunk ranges:\n%s", d)
	}
	// Changes closer than 2*Context lines merge into one hunk.
	b = append([]string(nil), a...)
	b[10], b[15] = "x\n", "y\n"
	d = Unified("f", strings.Join(a, ""), strings.Join(b, ""))
	if got := strings.Count(d, "@@ -"); got != 1 {
		t.Errorf("want 1 hunk for nearby changes, got %d:\n%s", got, d)
	}
}

func randText(r *rand.Rand, lines int, finalNewline bool) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		b.WriteString(string(rune('a' + r.Intn(4))))
		if i < lines-1 || finalNewline {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestUnifiedRoundTripRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 3000; n++ {
		a := randText(r, r.Intn(30), r.Intn(2) == 0)
		b := randText(r, r.Intn(30), r.Intn(2) == 0)
		d := Unified("f", a, b)
		if (a == b) != (d == "") {
			t.Fatalf("a == b is %v but diff is %q\na=%q b=%q", a == b, d, a, b)
		}
		if got := applyUnified(t, a, d); got != b {
			t.Fatalf("round trip failed\n a=%q\n b=%q\n got=%q\ndiff:\n%s", a, b, got, d)
		}
	}
}

func TestUnifiedSmallEditsInBigFiles(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	a := randText(r, 5000, true)
	lines := strings.SplitAfter(a, "\n")
	lines[1000] = "edited\n"
	lines = append(lines[:3000], lines[3010:]...)
	b := strings.Join(lines, "")
	d := Unified("f", a, b)
	if got := applyUnified(t, a, d); got != b {
		t.Fatal("round trip failed for a large file")
	}
	if n := strings.Count(d, "\n"); n > 120 {
		t.Errorf("diff of two small edits has %d lines", n)
	}
}

func TestUnifiedHugeEditFallsBackToBlock(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	a := randText(r, 3000, true)
	b := randText(r, 3000, true)
	d := Unified("f", a, b)
	if got := applyUnified(t, a, d); got != b {
		t.Fatal("round trip failed for a total rewrite")
	}
}

func FuzzUnified(f *testing.F) {
	f.Add("a\nb\nc\n", "a\nc\n")
	f.Add("", "x")
	f.Add("x", "")
	f.Add("a\r\nb", "a\nb\n")
	f.Fuzz(func(t *testing.T, a, b string) {
		d := Unified("f", a, b)
		if got := applyUnified(t, a, d); got != b {
			t.Fatalf("round trip failed\n a=%q\n b=%q\n got=%q\ndiff:\n%s", a, b, got, d)
		}
	})
}

func TestUnifiedFallsBackWhenTheWorkBudgetIsSpent(t *testing.T) {
	old := maxWork
	maxWork = 50
	defer func() { maxWork = old }()
	r := rand.New(rand.NewSource(5))
	for n := 0; n < 200; n++ {
		a := randText(r, 40+r.Intn(60), true)
		b := randText(r, 40+r.Intn(60), r.Intn(2) == 0)
		if got := applyUnified(t, a, Unified("f", a, b)); got != b {
			t.Fatalf("the budget fallback broke the diff\n a=%q\n b=%q", a, b)
		}
	}
}
