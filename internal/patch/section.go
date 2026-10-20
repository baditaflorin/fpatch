package patch

import (
	"fmt"
	"strings"

	"github.com/baditaflorin/fpatch/internal/spec"
)

type heading struct {
	start int // offset of the heading line
	line  int // 1-based line number
	level int
	text  string
}

// headings returns the ATX headings of text, skipping fenced code blocks.
func headings(text string, ix lineIndex) []heading {
	var out []heading
	var fenceCh byte
	fenceLen := 0
	for i, start := range ix.starts {
		end := len(text)
		if i+1 < len(ix.starts) {
			end = ix.starts[i+1] - 1
		}
		if start >= len(text) {
			break
		}
		line := text[start:end]
		trim := strings.TrimLeft(line, " \t")
		if lead := line[:len(line)-len(trim)]; len(lead) > 3 || strings.Contains(lead, "\t") {
			continue // indented code (four columns, a tab counts as four): neither a fence nor a heading
		}
		if fenceLen > 0 {
			if ch, n, rest := fence(trim); n >= fenceLen && ch == fenceCh && strings.TrimSpace(rest) == "" {
				fenceLen = 0
			}
			continue
		}
		if ch, n, _ := fence(trim); n >= 3 {
			fenceCh, fenceLen = ch, n
			continue
		}
		if level, txt, ok := spec.ParseHeading(trim); ok {
			out = append(out, heading{start: start, line: i + 1, level: level, text: normHeading(txt)})
		}
	}
	return out
}

// fence reports a run of ` or ~ at the start of s.
func fence(s string) (ch byte, n int, rest string) {
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return 0, 0, s
	}
	ch = s[0]
	for n < len(s) && s[n] == ch {
		n++
	}
	return ch, n, s[n:]
}

// normHeading drops an optional closing run of '#'.
func normHeading(t string) string {
	t = strings.TrimSpace(t)
	if strings.HasSuffix(t, "#") {
		u := strings.TrimRight(t, "#")
		if u == "" || strings.HasSuffix(u, " ") || strings.HasSuffix(u, "\t") {
			t = strings.TrimSpace(u)
		}
	}
	return t
}

func (a *applier) upsertSection(h spec.Hunk, hr *HunkResult) []edit {
	hr.Expected = 1
	level, want, _ := spec.ParseHeading(h.Heading)
	want = normHeading(want)
	var found []heading
	all := headings(a.text, a.ix)
	for _, hd := range all {
		if hd.level == level && hd.text == want {
			found = append(found, hd)
		}
	}
	hr.Matches = len(found)
	for _, f := range found {
		hr.Lines = append(hr.Lines, f.line)
	}
	body := trimTrailingBlank(h.New)

	switch len(found) {
	case 0:
		hr.Status = Applied
		hr.Message = "section added at the end of the file"
		add := ""
		t := a.text
		if t != "" {
			if !strings.HasSuffix(t, "\n") {
				add += "\n"
				t += "\n"
			}
			if t != "\n" && !strings.HasSuffix(t, "\n\n") {
				add += "\n"
			}
		}
		add += h.Heading + "\n" + joinLines(body)
		return []edit{{start: len(a.text), end: len(a.text), text: add}}
	case 1:
	default:
		hr.Status = Ambiguous
		hr.Message = fmt.Sprintf("the heading %q appears %d times (lines %s); fpatch will not guess which section to replace", h.Heading, len(found), joinInts(hr.Lines))
		return nil
	}

	f := found[0]
	end := len(a.text)
	for _, hd := range all {
		if hd.start > f.start && hd.level <= f.level {
			end = hd.start
			break
		}
	}
	old := a.text[f.start:end]
	headLine, rest := old, ""
	if i := strings.IndexByte(old, '\n'); i >= 0 {
		headLine, rest = old[:i], old[i+1:]
	}
	trailing := trailingBlank(rest)
	newSec := headLine + "\n" + joinLines(body) + trailing
	if end == len(a.text) && !strings.HasSuffix(old, "\n") && trailing == "" {
		newSec = strings.TrimSuffix(newSec, "\n") // keep a missing final newline missing
	}
	if newSec == old {
		hr.Status = AlreadyApplied
		hr.Message = "the section already has this content"
		return nil
	}
	hr.Status = Applied
	hr.Message = "section replaced"
	return []edit{{start: f.start, end: end, text: newSec}}
}

func trimTrailingBlank(lines []string) []string {
	n := len(lines)
	for n > 0 && strings.TrimSpace(lines[n-1]) == "" {
		n--
	}
	return lines[:n]
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// trailingBlank returns the longest suffix of s made of blank lines.
func trailingBlank(s string) string {
	end := len(s)
	for end > 0 {
		// Find the start of the last line of s[:end].
		body := strings.TrimSuffix(s[:end], "\n")
		start := strings.LastIndexByte(body, '\n') + 1
		if strings.TrimSpace(body[start:]) != "" {
			break
		}
		end = start
	}
	return s[end:]
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ", ")
}
