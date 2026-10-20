// Package diff renders unified diffs. It is a small Myers implementation so
// fpatch needs no external diff tool and no dependency.
package diff

import (
	"fmt"
	"strings"
)

// Context is the number of unchanged lines shown around each change.
const Context = 3

// maxEdits bounds the Myers search. Beyond it the changed region is shown as
// one block removed and one block added, which is still a correct diff.
const maxEdits = 2000

type opKind byte

const (
	keep opKind = iota
	del
	ins
)

type edit struct {
	kind opKind
	line string
}

// Unified returns the unified diff from a to b, with name in the header, or ""
// when the two are equal. A final line without a newline is marked with the
// usual "\ No newline at end of file".
func Unified(name, a, b string) string {
	if a == b {
		return ""
	}
	al, bl := splitLines(a), splitLines(b)
	edits := script(al, bl)
	return render(name, edits)
}

// splitLines splits s into lines that keep their terminating "\n"; a final
// line without one stays unterminated.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// script computes the edit script from a to b.
func script(a, b []string) []edit {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var out []edit
	for _, l := range a[:pre] {
		out = append(out, edit{keep, l})
	}
	out = append(out, myers(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		out = append(out, edit{keep, l})
	}
	return out
}

// myers is the greedy O(ND) algorithm. For each round d it saves the window of
// the furthest-reaching vector that the backtrack needs: indexes -d-1..d+1.
func myers(a, b []string) []edit {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 || m == 0 {
		return blockEdits(a, b)
	}
	max := n + m
	if max > maxEdits {
		max = maxEdits
	}
	off := max + 1
	v := make([]int, 2*max+4)
	trace := make([][]int, 0, 16)
	for d := 0; d <= max; d++ {
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(a, b, trace, d)
			}
		}
	}
	return blockEdits(a, b)
}

func blockEdits(a, b []string) []edit {
	out := make([]edit, 0, len(a)+len(b))
	for _, l := range a {
		out = append(out, edit{del, l})
	}
	for _, l := range b {
		out = append(out, edit{ins, l})
	}
	return out
}

func backtrack(a, b []string, trace [][]int, d int) []edit {
	x, y := len(a), len(b)
	var rev []edit
	for ; d > 0; d-- {
		w := trace[d] // w[k+d+1] is the vector value for diagonal k before round d
		k := x - y
		var pk int
		if k == -d || (k != d && w[k-1+d+1] < w[k+1+d+1]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := w[pk+d+1]
		py := px - pk
		for x > px && y > py {
			rev = append(rev, edit{keep, a[x-1]})
			x--
			y--
		}
		if x == px {
			rev = append(rev, edit{ins, b[y-1]})
			y--
		} else {
			rev = append(rev, edit{del, a[x-1]})
			x--
		}
	}
	for x > 0 && y > 0 {
		rev = append(rev, edit{keep, a[x-1]})
		x--
		y--
	}
	out := make([]edit, len(rev))
	for i, e := range rev {
		out[len(rev)-1-i] = e
	}
	return out
}

func render(name string, edits []edit) string {
	// Find changed positions and group them into hunks with Context lines.
	type span struct{ from, to int } // indexes into edits, to exclusive
	var spans []span
	for i := 0; i < len(edits); {
		if edits[i].kind == keep {
			i++
			continue
		}
		j := i
		for j < len(edits) && edits[j].kind != keep {
			j++
		}
		from, to := i-Context, j+Context
		if from < 0 {
			from = 0
		}
		if to > len(edits) {
			to = len(edits)
		}
		if n := len(spans); n > 0 && from <= spans[n-1].to {
			spans[n-1].to = to
		} else {
			spans = append(spans, span{from, to})
		}
		i = j
	}
	// Spans were extended by Context on both sides; merge the ones that touch
	// again after the extension of their neighbour.
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", name, name)
	// Line numbers before each edit index.
	aLine := make([]int, len(edits)+1)
	bLine := make([]int, len(edits)+1)
	a, bb := 1, 1
	for i, e := range edits {
		aLine[i], bLine[i] = a, bb
		if e.kind != ins {
			a++
		}
		if e.kind != del {
			bb++
		}
	}
	aLine[len(edits)], bLine[len(edits)] = a, bb
	for _, s := range spans {
		var aCount, bCount int
		for _, e := range edits[s.from:s.to] {
			if e.kind != ins {
				aCount++
			}
			if e.kind != del {
				bCount++
			}
		}
		aStart, bStart := aLine[s.from], bLine[s.from]
		if aCount == 0 {
			aStart--
		}
		if bCount == 0 {
			bStart--
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", rng(aStart, aCount), rng(bStart, bCount))
		for _, e := range edits[s.from:s.to] {
			prefix := " "
			switch e.kind {
			case del:
				prefix = "-"
			case ins:
				prefix = "+"
			}
			b.WriteString(prefix + e.line)
			if !strings.HasSuffix(e.line, "\n") {
				b.WriteString("\n\\ No newline at end of file\n")
			}
		}
	}
	return b.String()
}

func rng(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
