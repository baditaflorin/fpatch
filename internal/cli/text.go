package cli

import (
	"fmt"
	"strings"

	"github.com/baditaflorin/fpatch/internal/engine"
	"github.com/baditaflorin/fpatch/internal/patch"
)

// printText writes the human-readable report. Diffs and success lines go to
// stdout; everything that explains a failure goes to stderr.
func (c *command) printText(rep engine.Report, target string) {
	out, errw := c.env.Stdout, c.env.Stderr
	label := func(path string) string {
		if strings.HasPrefix(target, "ssh:") {
			return strings.TrimPrefix(target, "ssh:") + ":" + path
		}
		return path
	}
	var hunksApplied, filesChanged, failures int
	for _, f := range rep.Files {
		name := label(f.Path)
		// A diff means "this is what changed"; for a run that wrote nothing
		// because something failed, it would only mislead.
		if f.Diff != "" && !c.o.quiet && (f.Status == engine.StatusApplied || f.Status == engine.StatusWouldApply) {
			fmt.Fprint(out, f.Diff)
		}
		if f.Changed {
			filesChanged++
		}
		for _, h := range f.Hunks {
			prefix := fmt.Sprintf("%s: hunk %d (%s, spec line %d)", name, h.Index, h.Op, h.SpecLine)
			switch {
			case h.Status == patch.Applied:
				hunksApplied++
			case h.Status == patch.AlreadyApplied || h.Status == patch.NoOp:
				fmt.Fprintf(out, "fpatch: %s: %s\n", prefix, h.Message)
			case h.Status.Failed():
				failures++
				c.explainFailure(prefix, h)
			}
		}
		if f.Error != "" {
			failures++
			fmt.Fprintf(errw, "fpatch: %s: %s\n", name, f.Error)
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(errw, "fpatch: %s\n", n)
	}

	switch rep.ExitCode {
	case engine.ExitApplied:
		if rep.DryRun {
			fmt.Fprintf(out, "fpatch: dry run, nothing written: %s would change %s\n", count(hunksApplied, "hunk"), count(filesChanged, "file"))
		} else {
			fmt.Fprintf(out, "fpatch: applied %s in %s\n", count(hunksApplied, "hunk"), count(filesChanged, "file"))
		}
	case engine.ExitNothing:
		fmt.Fprintln(out, "fpatch: nothing to do: every hunk is already applied or changes nothing")
	case engine.ExitMiss:
		fmt.Fprintf(errw, "fpatch: nothing was written: %s did not match as demanded\n", count(failures, "hunk"))
	case engine.ExitIO:
		wrote := false
		for _, f := range rep.Files {
			if f.Status == engine.StatusRolledBack || f.Status == engine.StatusLeftModified {
				wrote = true
			}
		}
		if wrote {
			fmt.Fprintln(errw, "fpatch: the run failed while writing; see the notes above for the state of each file")
		} else {
			fmt.Fprintln(errw, "fpatch: nothing was written")
		}
	}
}

func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (c *command) explainFailure(prefix string, h patch.HunkResult) {
	w := c.env.Stderr
	fmt.Fprintf(w, "fpatch: %s: %s\n", prefix, h.Message)
	if len(h.Lines) > 0 {
		strs := make([]string, len(h.Lines))
		for i, l := range h.Lines {
			strs[i] = fmt.Sprint(l)
		}
		more := ""
		if h.Matches > len(h.Lines) {
			more = ", ..."
		}
		fmt.Fprintf(w, "  matches start at line %s%s\n", strings.Join(strs, ", "), more)
	}
	if n := h.Nearest; n != nil {
		fmt.Fprintf(w, "  nearest match: lines %d-%d, similarity %.2f:\n", n.StartLine, n.EndLine, n.Similarity)
		width := len(fmt.Sprint(n.EndLine))
		for i, l := range n.Lines {
			fmt.Fprintf(w, "    %*d | %s\n", width, n.StartLine+i, l)
		}
	} else if h.Status == patch.Missing {
		fmt.Fprintln(w, "  nothing in the file looks similar to the anchor")
	}
	for _, hint := range h.Hints {
		fmt.Fprintf(w, "  hint: %s\n", hint)
	}
}
