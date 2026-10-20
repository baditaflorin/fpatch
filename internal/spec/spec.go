// Package spec parses the patch specification fpatch reads from stdin.
//
// A spec is a list of blocks. Every directive line starts at column 0 with the
// marker "===" (changeable with Options.Marker) followed by a keyword:
//
//	=== replace [count=N|all]     anchor text ... === with ... new text ... === end
//	=== insert-after [count=N|all]  anchor ... === with ... lines to insert ... === end
//	=== insert-before [count=N|all] anchor ... === with ... lines to insert ... === end
//	=== append                   lines to append ... === end
//	=== upsert-section "## Heading"  section body ... === end
//	=== file PATH                (optional) the hunks that follow apply to PATH only
//
// Outside blocks, blank lines and lines starting with "#" are ignored.
package spec

import (
	"fmt"
	"strings"
)

// Op is the kind of a hunk.
type Op string

const (
	OpReplace       Op = "replace"
	OpInsertAfter   Op = "insert-after"
	OpInsertBefore  Op = "insert-before"
	OpAppend        Op = "append"
	OpUpsertSection Op = "upsert-section"
)

// DefaultMarker starts every directive line.
const DefaultMarker = "==="

// Hunk is one edit.
type Hunk struct {
	Op Op
	// Index is the 1-based position of the hunk in the spec.
	Index int
	// Line is the 1-based spec line of the hunk's first directive.
	Line int
	// File is the path of a preceding "=== file" directive, or "".
	File string
	// Count is how many times the anchor must match (replace, insert-*); it is
	// 1 unless the spec says otherwise. All means "one or more, every one".
	Count int
	All   bool
	// Old holds the anchor lines (replace, insert-*); New the replacement, the
	// lines to insert or append, or the section body.
	Old []string
	New []string
	// Heading is the section heading of upsert-section, such as "## Install".
	Heading string
}

// OldText is the anchor text: the anchor lines joined with newlines.
func (h Hunk) OldText() string { return strings.Join(h.Old, "\n") }

// NewText is the replacement text: the lines joined with newlines.
func (h Hunk) NewText() string { return strings.Join(h.New, "\n") }

// Spec is a parsed specification.
type Spec struct {
	Hunks []Hunk
	// Scoped is true when the spec uses "=== file" directives; every hunk then
	// has File set. Otherwise every hunk applies to every file.
	Scoped bool
}

// Files returns the paths named by "=== file" directives, in order of first
// appearance. It is empty for an unscoped spec.
func (s *Spec) Files() []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range s.Hunks {
		if h.File != "" && !seen[h.File] {
			seen[h.File] = true
			out = append(out, h.File)
		}
	}
	return out
}

// HunksFor returns the hunks that apply to path, in spec order.
func (s *Spec) HunksFor(path string) []Hunk {
	if !s.Scoped {
		return s.Hunks
	}
	var out []Hunk
	for _, h := range s.Hunks {
		if h.File == path {
			out = append(out, h)
		}
	}
	return out
}

// SyntaxError is a problem in the spec text.
type SyntaxError struct {
	Line int // 1-based; 0 when the error is about the spec as a whole
	Msg  string
}

func (e *SyntaxError) Error() string {
	if e.Line == 0 {
		return "spec: " + e.Msg
	}
	return fmt.Sprintf("spec line %d: %s", e.Line, e.Msg)
}
