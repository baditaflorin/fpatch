// Package engine runs a parsed spec against a set of files on an fsys.FS with
// all-or-nothing semantics: every file is read and every hunk is checked
// before the first byte is written.
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/internal/diff"
	"github.com/baditaflorin/fpatch/internal/patch"
	"github.com/baditaflorin/fpatch/internal/spec"
)

// Exit codes of the fpatch command.
const (
	ExitApplied  = 0 // changes written (or, with --dry-run, changes computed)
	ExitMiss     = 1 // an anchor is missing or ambiguous; nothing was written
	ExitUsage    = 2 // bad flags or spec; nothing was done
	ExitIO       = 3 // an I/O error; nothing was written, or the run was rolled back
	ExitNothing  = 4 // the spec is valid but there is nothing to change
	BackupSuffix = ".bak"
)

// File statuses.
const (
	StatusApplied      = "applied"       // changes written
	StatusWouldApply   = "would-apply"   // dry run: this file would change
	StatusUnchanged    = "unchanged"     // nothing to change in this file
	StatusFailed       = "failed"        // a hunk did not match, or the file could not be read or written
	StatusNotWritten   = "not-written"   // fine by itself, but the run stopped because of another file
	StatusRolledBack   = "rolled-back"   // written, then restored after a later write failed
	StatusLeftModified = "left-modified" // written, and restoring it failed: it holds the new content
)

// Options are the switches of a run.
type Options struct {
	DryRun     bool
	Backup     bool // keep the previous content of every changed file at FILE.bak
	Idempotent bool // an already-applied hunk counts as success
	NoDiff     bool // do not render diffs (they cost time on huge files and may quote secrets)
}

// Request is one run.
type Request struct {
	Files []string
	Spec  *spec.Spec
	Options
}

// FileReport is the outcome for one file.
type FileReport struct {
	Path       string             `json:"file"`
	Status     string             `json:"status"`
	Changed    bool               `json:"changed"`
	Written    bool               `json:"written"`
	LineEnding string             `json:"line_ending,omitempty"`
	Hunks      []patch.HunkResult `json:"hunks"`
	Diff       string             `json:"diff,omitempty"`
	Backup     string             `json:"backup,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// Report is the outcome of a run.
type Report struct {
	Files    []FileReport
	ExitCode int
	DryRun   bool
	// Notes carry run-level messages, such as what a rollback did.
	Notes []string
}

type pending struct {
	idx     int
	path    string
	version string
	orig    []byte
	content []byte
}

// Run executes req on fs. It never writes anything unless every hunk of every
// file matched. If a write fails part-way, the files already replaced are put
// back from the copies held in memory.
func Run(ctx context.Context, fs fsys.FS, req Request) Report {
	rep := Report{DryRun: req.DryRun, Files: make([]FileReport, len(req.Files))}
	var changed []pending
	hunkFail, ioFail := false, false

	for i, path := range req.Files {
		fr := FileReport{Path: path, Hunks: []patch.HunkResult{}}
		hunks := req.Spec.HunksFor(path)
		if len(hunks) == 0 {
			fr.Status = StatusUnchanged
			rep.Files[i] = fr
			continue
		}
		f, err := fs.Read(ctx, path)
		if err == nil {
			if off := bytes.IndexByte(f.Data, 0); off >= 0 {
				err = fmt.Errorf("refusing to patch a binary file (NUL byte at offset %d)", off)
			}
		}
		if err != nil {
			fr.Status = StatusFailed
			fr.Error = cleanError(path, err)
			ioFail = true
			rep.Files[i] = fr
			continue
		}
		res := patch.Apply(f.Data, hunks, patch.Options{Idempotent: req.Idempotent})
		fr.Hunks = res.Hunks
		fr.LineEnding = res.LineEnding
		switch {
		case res.Failed:
			fr.Status = StatusFailed
			hunkFail = true
		case !res.Changed:
			fr.Status = StatusUnchanged
		default:
			fr.Changed = true
			if !req.NoDiff {
				fr.Diff = diff.Unified(path, res.Before, res.After)
			}
			changed = append(changed, pending{idx: i, path: path, version: f.Version, orig: f.Data, content: res.Content})
		}
		rep.Files[i] = fr
	}

	if hunkFail || ioFail {
		for _, p := range changed {
			rep.Files[p.idx].Status = StatusNotWritten
		}
		rep.ExitCode = ExitMiss
		if ioFail {
			rep.ExitCode = ExitIO
		}
		return rep
	}
	if len(changed) == 0 {
		rep.ExitCode = ExitNothing
		return rep
	}
	if req.DryRun {
		for _, p := range changed {
			rep.Files[p.idx].Status = StatusWouldApply
		}
		return rep
	}
	rep.write(ctx, fs, req, changed)
	return rep
}

func (rep *Report) write(ctx context.Context, fs fsys.FS, req Request, changed []pending) {
	for n, p := range changed {
		fr := &rep.Files[p.idx]
		opts := fsys.WriteOptions{ExpectVersion: p.version}
		if req.Backup {
			opts.BackupSuffix = BackupSuffix
		}
		err := fs.Write(ctx, p.path, p.content, opts)
		if err == nil {
			fr.Status = StatusApplied
			fr.Written = true
			if req.Backup {
				fr.Backup = p.path + BackupSuffix
			}
			continue
		}
		fr.Status = StatusFailed
		fr.Error = cleanError(p.path, err)
		if errors.Is(err, fsys.ErrConflict) {
			fr.Error += " (another process modified it during the run)"
		}
		rep.ExitCode = ExitIO
		for _, q := range changed[n+1:] {
			rep.Files[q.idx].Status = StatusNotWritten
		}
		ctx, cancel := recoveryContext(ctx)
		defer cancel()
		undo := changed[:n]
		// After a lost connection the failed write may have gone through. Look
		// at the file before deciding: restore it if it holds the patched
		// content, leave it alone otherwise.
		if errors.Is(err, fsys.ErrConnection) && rep.settle(ctx, fs, p) {
			undo = changed[:n+1]
		}
		rep.rollback(ctx, fs, undo)
		return
	}
}

// recoveryContext gives the recovery steps their own time budget, so they run
// even when the run's context has already expired (a timeout, say).
func recoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// settle looks at a file whose write failed with an unknown outcome. It reports
// whether the file holds the patched content (and so must be restored); in every
// other case it records what it found.
func (rep *Report) settle(ctx context.Context, fs fsys.FS, p pending) bool {
	cur, err := fs.Read(ctx, p.path)
	switch {
	case err != nil:
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s: the connection failed during the write and the file could not be read back (%v): it may or may not hold the patched content", p.path, err))
	case bytes.Equal(cur.Data, p.content):
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s: the connection failed during the write, but the file holds the patched content", p.path))
		return true
	case bytes.Equal(cur.Data, p.orig):
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s: the connection failed during the write; the file was not replaced", p.path))
	default:
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s: the connection failed during the write and the file now holds neither the original nor the patched content (someone else changed it); it was left alone", p.path))
	}
	return false
}

// cleanError is the text of err without the path (and the failed operation),
// which the report prints once, in front of it, together with the host.
func cleanError(path string, err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return strings.TrimPrefix(err.Error(), path+": ")
}

// rollback restores the files in done to their original content, last first.
// ctx should come from recoveryContext.
func (rep *Report) rollback(ctx context.Context, fs fsys.FS, done []pending) {
	for i := len(done) - 1; i >= 0; i-- {
		p := done[i]
		fr := &rep.Files[p.idx]
		if err := fs.Write(ctx, p.path, p.orig, fsys.WriteOptions{}); err != nil {
			fr.Status = StatusLeftModified
			fr.Error = "could not be restored: " + err.Error()
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s was written and could NOT be restored (%v): it now holds the patched content", p.path, err))
			continue
		}
		fr.Status = StatusRolledBack
		fr.Written = false
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s was restored to its original content", p.path))
		if fr.Backup != "" {
			rep.Notes = append(rep.Notes, fmt.Sprintf("the backup %s was left in place", fr.Backup))
		}
	}
}
