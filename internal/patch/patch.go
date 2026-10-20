// Package patch applies parsed hunks to the text of one file.
//
// The rules, which docs and tests rely on:
//
//   - Every hunk is matched against the file as it was before the run, never
//     against the result of an earlier hunk, so line numbers in reports always
//     refer to the file on disk and hunk order does not matter. Two hunks must
//     not touch overlapping text; that is reported, not guessed at.
//   - A hunk that does not match exactly as often as it should changes
//     nothing, and neither does any other hunk of the same file (Apply either
//     returns a complete new content or none).
//   - CRLF files are matched as LF and written back as CRLF; a UTF-8 byte order
//     mark is kept; a missing final newline is kept.
package patch

import (
	"fmt"
	"sort"
	"strings"

	"github.com/baditaflorin/fpatch/internal/spec"
)

// Status is the outcome of one hunk.
type Status string

const (
	Applied        Status = "applied"
	AlreadyApplied Status = "already-applied"
	NoOp           Status = "no-op"
	Missing        Status = "missing"
	Ambiguous      Status = "ambiguous"
	CountMismatch  Status = "count-mismatch"
	Overlap        Status = "overlap"
)

// Failed reports whether the status stops the run.
func (s Status) Failed() bool {
	switch s {
	case Missing, Ambiguous, CountMismatch, Overlap:
		return true
	}
	return false
}

// Nearest is the part of the file that looks most like an anchor that did not
// match.
type Nearest struct {
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	Similarity float64  `json:"similarity"`
	Lines      []string `json:"lines"`
}

// HunkResult describes what happened to one hunk.
type HunkResult struct {
	Index    int      `json:"index"`
	Op       string   `json:"op"`
	SpecLine int      `json:"spec_line"`
	Status   Status   `json:"status"`
	Matches  int      `json:"matches"`
	Expected int      `json:"expected"`
	All      bool     `json:"all,omitempty"`
	Lines    []int    `json:"lines,omitempty"`
	Nearest  *Nearest `json:"nearest,omitempty"`
	Hints    []string `json:"hints,omitempty"`
	Message  string   `json:"message,omitempty"`
}

// Options tune Apply.
type Options struct {
	// Idempotent treats a hunk whose effect is already present as success.
	Idempotent bool
}

// Result is the outcome of Apply for one file.
type Result struct {
	Hunks  []HunkResult
	Failed bool
	// Content is the new file content; nil when Failed.
	Content []byte
	// Before and After are the LF-normalised texts (without BOM) a diff should
	// compare.
	Before, After string
	// Changed is true when Content differs from the input.
	Changed bool
	// LineEnding is "lf", "crlf" or "mixed".
	LineEnding string
}

type edit struct {
	start, end int
	text       string
	hunk       int // index into Result.Hunks
}

type applier struct {
	text   string
	ix     lineIndex
	opt    Options
	ending string
	grams  *grams // lazily built by explainMiss
}

const bom = "\xef\xbb\xbf"

// Apply applies hunks to src.
func Apply(src []byte, hunks []spec.Hunk, opt Options) Result {
	text := string(src)
	hasBOM := strings.HasPrefix(text, bom)
	text = strings.TrimPrefix(text, bom)
	ending := "lf"
	if crlf := strings.Count(text, "\r\n"); crlf > 0 {
		if crlf == strings.Count(text, "\n") {
			ending = "crlf"
			text = strings.ReplaceAll(text, "\r\n", "\n")
		} else {
			ending = "mixed"
		}
	}
	a := &applier{text: text, ix: newLineIndex(text), opt: opt, ending: ending}
	res := Result{Before: text, LineEnding: ending}
	var edits []edit
	for _, h := range hunks {
		hr := HunkResult{Index: h.Index, Op: string(h.Op), SpecLine: h.Line, Expected: h.Count, All: h.All}
		if h.All {
			hr.Expected = 0
		}
		pos := len(res.Hunks)
		var es []edit
		switch h.Op {
		case spec.OpReplace:
			es = a.replace(h, &hr)
		case spec.OpInsertAfter, spec.OpInsertBefore:
			es = a.insert(h, &hr)
		case spec.OpAppend:
			es = a.appendText(h, &hr)
		case spec.OpUpsertSection:
			es = a.upsertSection(h, &hr)
		default:
			hr.Status = Missing
			hr.Message = fmt.Sprintf("unknown operation %q", h.Op)
		}
		for i := range es {
			es[i].hunk = pos
		}
		edits = append(edits, es...)
		res.Hunks = append(res.Hunks, hr)
		if hr.Status.Failed() {
			res.Failed = true
		}
	}
	if res.Failed {
		return res
	}

	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start < edits[j].start
		}
		return edits[i].end < edits[j].end
	})
	lastEnd, lastHunk := 0, -1
	for _, e := range edits {
		if lastHunk >= 0 && e.start < lastEnd && e.hunk != lastHunk {
			hr := &res.Hunks[e.hunk]
			hr.Status = Overlap
			hr.Message = fmt.Sprintf("overlaps hunk %d; hunks are matched against the original file and must not touch the same text", res.Hunks[lastHunk].Index)
			res.Failed = true
		}
		if e.end > lastEnd || lastHunk < 0 {
			lastEnd, lastHunk = e.end, e.hunk
		}
	}
	if res.Failed {
		return res
	}

	var b strings.Builder
	pos := 0
	for _, e := range edits {
		b.WriteString(text[pos:e.start])
		b.WriteString(e.text)
		pos = e.end
	}
	b.WriteString(text[pos:])
	after := b.String()
	res.After = after
	res.Changed = after != text
	out := after
	if ending == "crlf" {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if hasBOM {
		out = bom + out
	}
	res.Content = []byte(out)
	return res
}

// findAll returns the start offsets of the non-overlapping occurrences of sub.
func findAll(s, sub string) []int {
	if sub == "" {
		return nil
	}
	var out []int
	for i := 0; ; {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			return out
		}
		out = append(out, i+j)
		i += j + len(sub)
	}
}

// checkCount fills the match fields of hr and reports whether the number of
// matches is what the hunk demands.
func (a *applier) checkCount(h spec.Hunk, hr *HunkResult, starts []int) bool {
	n := len(starts)
	hr.Matches = n
	for i, s := range starts {
		if i == 20 {
			break
		}
		hr.Lines = append(hr.Lines, a.ix.line(s))
	}
	switch {
	case h.All && n >= 1, !h.All && n == h.Count:
		return true
	case n == 0:
		hr.Status = Missing
		if h.All {
			hr.Message = "anchor not found (count=all needs at least one match)"
		} else {
			hr.Message = fmt.Sprintf("anchor not found (expected %d match%s)", h.Count, plural(h.Count))
		}
		a.explainMiss(h.OldText(), hr)
	case n > h.Count:
		hr.Status = Ambiguous
		hr.Message = fmt.Sprintf("anchor matches %d times (expected %d); make it longer so it is unique, or set count=%d / count=all", n, h.Count, n)
	default:
		hr.Status = CountMismatch
		hr.Message = fmt.Sprintf("anchor matches %d time%s but count=%d was demanded", n, plural(n), h.Count)
	}
	return false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func (a *applier) explainMiss(anchor string, hr *HunkResult) {
	if a.grams == nil {
		a.grams = newGrams(a.text) // built once, however many hunks miss
	}
	hr.Nearest = a.grams.nearest(anchor)
	hr.Hints = hints(a.text, anchor, a.ending)
}

func (a *applier) replace(h spec.Hunk, hr *HunkResult) []edit {
	old, nw := h.OldText(), h.NewText()
	starts := findAll(a.text, old)
	want := h.Count
	if h.All {
		want = 1
	}
	if a.opt.Idempotent && old != nw {
		applied := false
		switch {
		case nw == "":
			applied = len(starts) == 0
		case strings.Contains(nw, old):
			// The old text lives on inside the new text, so "old is gone" can
			// never be the signal; look for the new text itself.
			applied = len(findAll(a.text, nw)) >= want
		default:
			applied = len(starts) == 0 && len(findAll(a.text, nw)) >= want
		}
		if applied {
			hr.Status = AlreadyApplied
			hr.Matches = len(findAll(a.text, nw))
			hr.Message = "already applied"
			return nil
		}
	}
	if !a.checkCount(h, hr, starts) {
		return nil
	}
	if old == nw {
		hr.Status = NoOp
		hr.Message = "the old and the new text are identical"
		return nil
	}
	hr.Status = Applied
	es := make([]edit, 0, len(starts))
	for _, s := range starts {
		es = append(es, edit{start: s, end: s + len(old), text: nw})
	}
	return es
}

func (a *applier) insert(h spec.Hunk, hr *HunkResult) []edit {
	anchor := h.OldText()
	starts := findAll(a.text, anchor)
	if !a.checkCount(h, hr, starts) {
		return nil
	}
	body := h.NewText()
	var es []edit
	seen := map[int]bool{} // several matches on one line insert once
	for _, s := range starts {
		at := a.insertionPoint(h.Op == spec.OpInsertAfter, s, s+len(anchor))
		if seen[at] {
			continue
		}
		seen[at] = true
		if a.opt.Idempotent && a.alreadyThere(h.Op == spec.OpInsertAfter, at, body) {
			continue
		}
		text := body + "\n"
		if at == len(a.text) && at > 0 && a.text[at-1] != '\n' {
			text = "\n" + body // keep a missing final newline missing
		}
		es = append(es, edit{start: at, end: at, text: text})
	}
	if len(es) == 0 {
		hr.Status = AlreadyApplied
		hr.Message = "the lines are already there"
	} else {
		hr.Status = Applied
	}
	return es
}

// insertionPoint returns the offset at which whole lines are inserted: after
// the line holding the end of the match, or before the line holding its start.
func (a *applier) insertionPoint(after bool, start, end int) int {
	if !after {
		return strings.LastIndexByte(a.text[:start], '\n') + 1
	}
	if end > start && a.text[end-1] == '\n' {
		return end
	}
	i := strings.IndexByte(a.text[end:], '\n')
	if i < 0 {
		return len(a.text)
	}
	return end + i + 1
}

func (a *applier) alreadyThere(after bool, at int, body string) bool {
	if after {
		rest := a.text[at:]
		// "rest == body" is the form at the end of a file with no final
		// newline; an empty body has no such form (nothing follows).
		return strings.HasPrefix(rest, body+"\n") || (body != "" && rest == body)
	}
	pre := a.text[:at]
	// The lines must start on a line boundary: "xfoo\n" does not hold "foo\n".
	return strings.HasSuffix(pre, body+"\n") && (len(pre) == len(body)+1 || pre[len(pre)-len(body)-2] == '\n')
}

func (a *applier) appendText(h spec.Hunk, hr *HunkResult) []edit {
	hr.Expected = 0
	body := h.NewText()
	if a.opt.Idempotent && endsWithBlock(a.text, body) {
		hr.Status = AlreadyApplied
		hr.Message = "the lines are already at the end of the file"
		return nil
	}
	add := body + "\n"
	if len(a.text) > 0 && !strings.HasSuffix(a.text, "\n") {
		add = "\n" + add
	}
	hr.Status = Applied
	return []edit{{start: len(a.text), end: len(a.text), text: add}}
}

// endsWithBlock reports whether text ends with body as whole lines.
func endsWithBlock(text, body string) bool {
	t := strings.TrimSuffix(text, "\n")
	if !strings.HasSuffix(t, body) {
		return false
	}
	return len(t) == len(body) || t[len(t)-len(body)-1] == '\n'
}

// lineIndex maps byte offsets to line numbers.
type lineIndex struct{ starts []int }

func newLineIndex(s string) lineIndex {
	starts := []int{0}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return lineIndex{starts}
}

// line returns the 1-based line number containing offset off.
func (ix lineIndex) line(off int) int {
	return sort.Search(len(ix.starts), func(i int) bool { return ix.starts[i] > off })
}
