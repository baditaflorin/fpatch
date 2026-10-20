// Package cli is the fpatch command line: flags, spec input, output formats
// and exit codes. cmd/fpatch only wires it to the process.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/internal/engine"
	"github.com/baditaflorin/fpatch/internal/spec"
)

// maxSpecSize bounds how much spec text fpatch reads.
const maxSpecSize = 16 << 20

// Env is everything the command line needs from its process.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// StdinIsTerminal is true when stdin is an interactive terminal, where
	// waiting for a spec would just hang.
	StdinIsTerminal bool
	// Version is printed by --version.
	Version string
}

const usage = `fpatch - exact-match, atomic, all-or-nothing file patcher

Usage:
  fpatch FILE... [flags] < spec

The spec is read from stdin (heredoc-friendly). Every hunk must match exactly
as many times as it says (default 1) or nothing is written, in any file.

Spec blocks (directive lines start at column 0):
  === replace [count=N|all]        old text / === with / new text / === end
  === insert-after [count=N|all]   anchor / === with / lines to insert / === end
  === insert-before [count=N|all]  anchor / === with / lines to insert / === end
  === append                       lines to append / === end
  === upsert-section "## Heading"  section body / === end
  === file PATH                    hunks below apply to PATH only (a FILE argument)

Flags:
  --host ALIAS      patch the files on a remote host over ssh (ssh_config alias)
  --fs TARGET       any registered file system, "name" or "name:arg" (e.g. ssh:web1)
  -n, --dry-run     show the diff, write nothing
  --json            print one JSON object per file, then a summary object
  --backup          keep the previous content of each changed file as FILE.bak
  --idempotent      a hunk that is already applied counts as success
  -q, --quiet       do not print diffs
  -f, --spec FILE   read the spec from FILE instead of stdin ("-" is stdin)
  --marker STR      directive marker instead of "===" (for specs that contain "=== end")
  --timeout DUR     give up after DUR (default 2m, 0 for no limit)
  --version         print the version number (nothing else)
  -h, --help        print this help

Exit codes:
  0  applied (with --dry-run: changes computed, nothing written)
  1  an anchor is missing or ambiguous; nothing was written
  2  usage or spec error
  3  I/O error; nothing was written, or the run was rolled back
  4  nothing to do: every hunk is already applied or a no-op
`

type options struct {
	host, fsTarget  string
	dryRun, jsonOut bool
	backup, idem    bool
	quiet           bool
	specFile        string
	marker          string
	timeout         time.Duration
	version, help   bool
	hostSet, fsSet  bool // given on the command line, even if empty
}

// Run executes the command line args (without the program name).
func Run(args []string, env Env) int {
	var o options
	fl := flag.NewFlagSet("fpatch", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	fl.StringVar(&o.host, "host", "", "")
	fl.StringVar(&o.fsTarget, "fs", "", "")
	fl.BoolVar(&o.dryRun, "dry-run", false, "")
	fl.BoolVar(&o.dryRun, "n", false, "")
	fl.BoolVar(&o.jsonOut, "json", false, "")
	fl.BoolVar(&o.backup, "backup", false, "")
	fl.BoolVar(&o.idem, "idempotent", false, "")
	fl.BoolVar(&o.quiet, "quiet", false, "")
	fl.BoolVar(&o.quiet, "q", false, "")
	fl.StringVar(&o.specFile, "spec", "", "")
	fl.StringVar(&o.specFile, "f", "", "")
	fl.StringVar(&o.marker, "marker", "", "")
	fl.DurationVar(&o.timeout, "timeout", 2*time.Minute, "")
	fl.BoolVar(&o.version, "version", false, "")
	fl.BoolVar(&o.help, "help", false, "")
	fl.BoolVar(&o.help, "h", false, "")

	c := &command{env: env, o: &o}
	files, err := parseArgs(fl, args)
	fl.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			o.hostSet = true
		case "fs":
			o.fsSet = true
		}
	})
	c.json = o.jsonOut
	if err != nil {
		code := c.usageError(err.Error())
		fmt.Fprintln(env.Stderr, "run fpatch --help for usage")
		return code
	}
	switch {
	case o.help:
		io.WriteString(env.Stdout, usage)
		return engine.ExitApplied
	case o.version:
		fmt.Fprintln(env.Stdout, env.Version)
		return engine.ExitApplied
	}
	return c.run(files)
}

// parseArgs parses flags that may appear before, between and after the file
// arguments; everything after a "--" is a file.
func parseArgs(fl *flag.FlagSet, args []string) (files []string, err error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	for len(args) > 0 {
		if err := fl.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return files, nil
			}
			return nil, err
		}
		args = fl.Args()
		if len(args) == 0 {
			break
		}
		files = append(files, args[0])
		args = args[1:]
	}
	return append(files, tail...), nil
}

type command struct {
	env  Env
	o    *options
	json bool
}

// usageError reports a mistake of the caller (exit code 2).
func (c *command) usageError(msg string) int {
	fmt.Fprintf(c.env.Stderr, "fpatch: %s\n", msg)
	if c.json {
		c.writeJSON(summary{Type: "summary", ExitCode: engine.ExitUsage, Error: msg})
	}
	return engine.ExitUsage
}

func (c *command) run(files []string) int {
	o := c.o
	if len(files) == 0 {
		return c.usageError("no files given (usage: fpatch FILE... < spec)")
	}
	seen := map[string]string{} // cleaned path -> as typed
	exact := map[string]bool{}
	for _, f := range files {
		if err := fsys.ValidatePath(f); err != nil {
			return c.usageError(err.Error())
		}
		key := filepath.Clean(f)
		if prev, dup := seen[key]; dup {
			return c.usageError(fmt.Sprintf("%q and %q are the same file", prev, f))
		}
		seen[key] = f
		exact[f] = true
	}
	// An empty value must not quietly fall back to the local disk: a script
	// with --host "$HOST" and HOST unset would patch the wrong machine.
	if o.hostSet && o.host == "" {
		return c.usageError("--host needs a host alias (an empty value would patch the local files instead)")
	}
	if o.fsSet && o.fsTarget == "" {
		return c.usageError("--fs needs a target such as ssh:web1 (use --fs local for the local disk)")
	}
	if o.host != "" && o.fsTarget != "" {
		return c.usageError("--host and --fs cannot be used together")
	}
	target := o.fsTarget
	if o.host != "" {
		target = "ssh:" + o.host
	}

	text, err := c.readSpec()
	if err != nil {
		return c.usageError(err.Error())
	}
	sp, err := spec.Parse(text, spec.Options{Marker: o.marker})
	if err != nil {
		return c.usageError(err.Error())
	}
	if sp.Scoped {
		for _, name := range sp.Files() {
			if !exact[name] {
				return c.usageError(fmt.Sprintf("the spec names %q, which is not one of the FILE arguments; the files to patch are only those on the command line", name))
			}
		}
	}

	fs, err := fsys.New(target)
	if err != nil {
		return c.usageError(err.Error())
	}
	defer fs.Close()

	ctx := context.Background()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	rep := engine.Run(ctx, fs, engine.Request{
		Files:   files,
		Spec:    sp,
		Options: engine.Options{DryRun: o.dryRun, Backup: o.backup, Idempotent: o.idem, NoDiff: o.quiet},
	})
	if err := ctx.Err(); err != nil && rep.ExitCode == engine.ExitIO {
		rep.Notes = append(rep.Notes, "the --timeout of "+o.timeout.String()+" expired")
	}
	if o.jsonOut {
		c.printJSON(rep)
	} else {
		c.printText(rep, target)
	}
	return rep.ExitCode
}

func (c *command) readSpec() (string, error) {
	var r io.Reader
	switch {
	case c.o.specFile != "" && c.o.specFile != "-":
		f, err := os.Open(c.o.specFile)
		if err != nil {
			return "", fmt.Errorf("cannot read the spec: %w", err)
		}
		defer f.Close()
		r = f
	case c.env.StdinIsTerminal:
		return "", errors.New("no spec on stdin: pipe or redirect one (a heredoc works), or use --spec FILE")
	default:
		r = c.env.Stdin
	}
	data, err := io.ReadAll(io.LimitReader(r, maxSpecSize+1))
	if err != nil {
		return "", fmt.Errorf("cannot read the spec: %w", err)
	}
	if len(data) > maxSpecSize {
		return "", fmt.Errorf("the spec is larger than %d MiB", maxSpecSize>>20)
	}
	return string(data), nil
}

type summary struct {
	Type     string   `json:"type"`
	ExitCode int      `json:"exit_code"`
	DryRun   bool     `json:"dry_run"`
	Files    int      `json:"files"`
	Changed  int      `json:"changed"`
	Written  int      `json:"written"`
	Notes    []string `json:"notes,omitempty"`
	Error    string   `json:"error,omitempty"`
}

type jsonFile struct {
	Type string `json:"type"`
	engine.FileReport
}

func (c *command) writeJSON(v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	c.env.Stdout.Write(buf.Bytes())
}

func (c *command) printJSON(rep engine.Report) {
	s := summary{Type: "summary", ExitCode: rep.ExitCode, DryRun: rep.DryRun, Files: len(rep.Files), Notes: rep.Notes}
	for _, f := range rep.Files {
		if c.o.quiet {
			f.Diff = ""
		}
		c.writeJSON(jsonFile{Type: "file", FileReport: f})
		if f.Changed {
			s.Changed++
		}
		if f.Written {
			s.Written++
		}
	}
	c.writeJSON(s)
}
