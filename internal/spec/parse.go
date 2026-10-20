package spec

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Options tune Parse.
type Options struct {
	// Marker replaces "===" as the start of directive lines, for specs whose
	// content contains lines such as "=== end". It must not contain whitespace.
	Marker string
}

var keywords = map[string]bool{
	"replace": true, "insert-after": true, "insert-before": true,
	"append": true, "upsert-section": true, "file": true,
	"with": true, "end": true,
}

// ParseHeading splits an ATX heading such as "## Install" into its level (the
// number of leading '#', 1 to 6) and its text. ok is false for anything else.
func ParseHeading(h string) (level int, text string, ok bool) {
	h = strings.TrimSpace(h)
	for level < len(h) && h[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level == len(h) {
		return 0, "", false
	}
	if h[level] != ' ' && h[level] != '\t' {
		return 0, "", false
	}
	text = strings.TrimSpace(h[level:])
	if text == "" {
		return 0, "", false
	}
	return level, text, true
}

type parser struct {
	marker string
	spec   Spec

	// open block
	open    bool
	cur     Hunk
	inNew   bool // replace/insert-*: past "=== with"
	file    string
	pending int // line of a "=== file" directive that has no hunk yet, or 0
}

// directive reports whether line is a directive line and returns its keyword
// and the rest of the line. Only the known keywords count, so a marker that
// also starts ordinary text (like "###") only claims real directives.
func (p *parser) directive(line string) (kw, rest string, ok bool) {
	t := strings.TrimRight(line, " \t")
	if !strings.HasPrefix(t, p.marker+" ") {
		return "", "", false
	}
	t = strings.TrimSpace(t[len(p.marker):])
	kw = t
	if i := strings.IndexAny(t, " \t"); i >= 0 {
		kw, rest = t[:i], strings.TrimSpace(t[i:])
	}
	if !keywords[kw] {
		return "", "", false
	}
	return kw, rest, true
}

func (p *parser) looksLikeDirective(line string) bool {
	return strings.HasPrefix(strings.TrimRight(line, " \t"), p.marker+" ")
}

// Parse parses spec text. The error, if any, is a *SyntaxError.
func Parse(text string, opt Options) (*Spec, error) {
	marker := opt.Marker
	if marker == "" {
		marker = DefaultMarker
	}
	if strings.ContainsAny(marker, " \t\r\n") {
		return nil, &SyntaxError{Msg: "the marker must not contain whitespace"}
	}
	if !utf8.ValidString(text) {
		return nil, &SyntaxError{Msg: "the spec is not valid UTF-8"}
	}
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}

	p := &parser{marker: marker}
	for i, line := range lines {
		if err := p.feed(i+1, line); err != nil {
			return nil, err
		}
	}
	if p.open {
		return nil, &SyntaxError{Line: p.cur.Line, Msg: fmt.Sprintf("the %s block is never closed: add %q", p.cur.Op, p.marker+" end")}
	}
	if p.pending != 0 {
		return nil, &SyntaxError{Line: p.pending, Msg: fmt.Sprintf("%q has no hunks", p.marker+" file "+p.file)}
	}
	if len(p.spec.Hunks) == 0 {
		return nil, &SyntaxError{Msg: "the spec has no hunks"}
	}
	return &p.spec, nil
}

func (p *parser) feed(n int, line string) error {
	kw, rest, isDir := p.directive(line)
	if !p.open {
		return p.top(n, line, kw, rest, isDir)
	}
	// Inside a block, only the expected terminator is a directive; any other
	// directive almost certainly means a "=== end" was forgotten.
	switch {
	case isDir && kw == "with" && !p.inNew && (p.cur.Op == OpReplace || p.cur.Op == OpInsertAfter || p.cur.Op == OpInsertBefore):
		p.inNew = true
		return nil
	case isDir && kw == "end":
		if (p.cur.Op == OpReplace || p.cur.Op == OpInsertAfter || p.cur.Op == OpInsertBefore) && !p.inNew {
			return &SyntaxError{Line: n, Msg: fmt.Sprintf("the %s block that starts at line %d needs %q before %q", p.cur.Op, p.cur.Line, p.marker+" with", p.marker+" end")}
		}
		return p.finish(n)
	case isDir:
		return &SyntaxError{Line: n, Msg: fmt.Sprintf("unexpected %q inside the %s block that starts at line %d; is a %q missing? (to use this line as literal text, pick another marker with --marker)", p.marker+" "+kw, p.cur.Op, p.cur.Line, p.marker+" end")}
	}
	if p.cur.Op == OpAppend || p.cur.Op == OpUpsertSection || p.inNew {
		p.cur.New = append(p.cur.New, line)
	} else {
		p.cur.Old = append(p.cur.Old, line)
	}
	return nil
}

func (p *parser) top(n int, line, kw, rest string, isDir bool) error {
	t := strings.TrimSpace(line)
	if !isDir {
		if t == "" || strings.HasPrefix(t, "#") && !p.looksLikeDirective(line) {
			return nil
		}
		if p.looksLikeDirective(line) {
			return &SyntaxError{Line: n, Msg: fmt.Sprintf("unknown directive %q", t)}
		}
		return &SyntaxError{Line: n, Msg: fmt.Sprintf("unexpected text outside a block: %q (a hunk starts with %q, %q, ...)", t, p.marker+" replace", p.marker+" append")}
	}
	switch kw {
	case "file":
		if rest == "" {
			return &SyntaxError{Line: n, Msg: fmt.Sprintf("%q needs a file path", p.marker+" file")}
		}
		if len(p.spec.Hunks) > 0 && !p.spec.Scoped {
			return &SyntaxError{Line: n, Msg: fmt.Sprintf("hunks before the first %q line would apply to every file; put every hunk under a %q line or none", p.marker+" file", p.marker+" file")}
		}
		if p.pending != 0 {
			return &SyntaxError{Line: p.pending, Msg: fmt.Sprintf("%q has no hunks", p.marker+" file "+p.file)}
		}
		p.spec.Scoped = true
		p.file = rest
		p.pending = n
		return nil
	case "with", "end":
		return &SyntaxError{Line: n, Msg: fmt.Sprintf("%q outside a block", p.marker+" "+kw)}
	}
	h := Hunk{Op: Op(kw), Line: n, File: p.file, Count: 1}
	switch h.Op {
	case OpReplace, OpInsertAfter, OpInsertBefore:
		for _, tok := range strings.Fields(rest) {
			v, ok := strings.CutPrefix(tok, "count=")
			if !ok {
				return &SyntaxError{Line: n, Msg: fmt.Sprintf("unknown option %q (expected count=N or count=all)", tok)}
			}
			if v == "all" {
				h.All = true
				continue
			}
			c, err := strconv.Atoi(v)
			if err != nil || c < 1 {
				return &SyntaxError{Line: n, Msg: fmt.Sprintf("bad count %q (expected a number from 1 up, or all)", v)}
			}
			h.Count = c
			h.All = false
		}
	case OpAppend:
		if rest != "" {
			return &SyntaxError{Line: n, Msg: fmt.Sprintf("%q takes no options", p.marker+" append")}
		}
	case OpUpsertSection:
		heading := rest
		if strings.HasPrefix(heading, `"`) {
			u, err := strconv.Unquote(heading)
			if err != nil {
				return &SyntaxError{Line: n, Msg: fmt.Sprintf("bad quoted heading %s", heading)}
			}
			heading = u
		}
		if _, _, ok := ParseHeading(heading); !ok {
			return &SyntaxError{Line: n, Msg: fmt.Sprintf("%q needs a Markdown heading such as %q, got %q", p.marker+" upsert-section", `"## Install"`, heading)}
		}
		h.Heading = strings.TrimSpace(heading)
	}
	p.open, p.cur, p.inNew = true, h, false
	return nil
}

// finish validates and stores the open block.
func (p *parser) finish(n int) error {
	h := p.cur
	switch h.Op {
	case OpReplace:
		if h.OldText() == "" {
			return &SyntaxError{Line: h.Line, Msg: "the text to replace is empty"}
		}
	case OpInsertAfter, OpInsertBefore:
		if h.OldText() == "" {
			return &SyntaxError{Line: h.Line, Msg: "the anchor text is empty"}
		}
		if len(h.New) == 0 {
			return &SyntaxError{Line: h.Line, Msg: fmt.Sprintf("%s has nothing to insert; add at least one line (a blank line is fine)", h.Op)}
		}
	case OpAppend:
		if len(h.New) == 0 {
			return &SyntaxError{Line: h.Line, Msg: "append has nothing to append; add at least one line (a blank line is fine)"}
		}
	}
	h.Index = len(p.spec.Hunks) + 1
	p.spec.Hunks = append(p.spec.Hunks, h)
	p.open = false
	p.pending = 0
	return nil
}

// Format renders s as spec text with the default marker. Parse(Format(s))
// yields a spec equal to s (apart from line numbers).
func Format(s *Spec) string {
	var b strings.Builder
	file := ""
	for _, h := range s.Hunks {
		if s.Scoped && h.File != file {
			file = h.File
			fmt.Fprintf(&b, "%s file %s\n", DefaultMarker, file)
		}
		fmt.Fprintf(&b, "%s %s", DefaultMarker, h.Op)
		switch h.Op {
		case OpReplace, OpInsertAfter, OpInsertBefore:
			if h.All {
				b.WriteString(" count=all")
			} else if h.Count != 1 {
				fmt.Fprintf(&b, " count=%d", h.Count)
			}
		case OpUpsertSection:
			b.WriteString(" " + strconv.Quote(h.Heading))
		}
		b.WriteByte('\n')
		for _, l := range h.Old {
			b.WriteString(l + "\n")
		}
		if h.Op == OpReplace || h.Op == OpInsertAfter || h.Op == OpInsertBefore {
			fmt.Fprintf(&b, "%s with\n", DefaultMarker)
		}
		for _, l := range h.New {
			b.WriteString(l + "\n")
		}
		fmt.Fprintf(&b, "%s end\n", DefaultMarker)
	}
	return b.String()
}
