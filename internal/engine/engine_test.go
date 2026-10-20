package engine_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/fsys/memfs"
	"github.com/baditaflorin/fpatch/internal/engine"
	"github.com/baditaflorin/fpatch/internal/spec"
)

func run(t *testing.T, fs fsys.FS, files []string, specText string, opts engine.Options) engine.Report {
	t.Helper()
	sp, err := spec.Parse(specText, spec.Options{})
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return engine.Run(context.Background(), fs, engine.Request{Files: files, Spec: sp, Options: opts})
}

func content(t *testing.T, m *memfs.FS, path string) string {
	t.Helper()
	d, _, ok := m.Get(path)
	if !ok {
		t.Fatalf("%s does not exist", path)
	}
	return string(d)
}

func statusOf(rep engine.Report) []string {
	var out []string
	for _, f := range rep.Files {
		out = append(out, f.Status)
	}
	return out
}

const swap = "=== replace\nold\n=== with\nnew\n=== end\n"

func TestAppliesToEveryFile(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old a\n"), 0o640)
	m.Put("b", []byte("x old y\n"), 0o755)
	rep := run(t, m, []string{"a", "b"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitApplied {
		t.Fatalf("exit %d: %+v", rep.ExitCode, rep)
	}
	if content(t, m, "a") != "new a\n" || content(t, m, "b") != "x new y\n" {
		t.Errorf("contents: %q %q", content(t, m, "a"), content(t, m, "b"))
	}
	if _, mode, _ := m.Get("b"); mode != 0o755 {
		t.Errorf("mode changed to %o", mode)
	}
	if got := statusOf(rep); !reflect.DeepEqual(got, []string{"applied", "applied"}) {
		t.Errorf("statuses = %v", got)
	}
	for _, f := range rep.Files {
		if !f.Written || !f.Changed || f.Diff == "" {
			t.Errorf("%s: written=%v changed=%v diff=%q", f.Path, f.Written, f.Changed, f.Diff)
		}
	}
}

func TestSecondHunkMissingWritesNothing(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old\n"), 0o644)
	rep := run(t, m, []string{"a"}, swap+"=== replace\nnope\n=== with\nx\n=== end\n", engine.Options{})
	if rep.ExitCode != engine.ExitMiss {
		t.Fatalf("exit = %d, want %d", rep.ExitCode, engine.ExitMiss)
	}
	if len(m.Writes()) != 0 || content(t, m, "a") != "old\n" {
		t.Errorf("the file was touched: writes=%v content=%q", m.Writes(), content(t, m, "a"))
	}
	if rep.Files[0].Status != engine.StatusFailed || rep.Files[0].Diff != "" {
		t.Errorf("file report: %+v", rep.Files[0])
	}
}

func TestSecondFileMissingLeavesTheFirstUntouched(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old\n"), 0o644)
	m.Put("b", []byte("nothing to see\n"), 0o644)
	rep := run(t, m, []string{"a", "b"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitMiss {
		t.Fatalf("exit = %d, want %d", rep.ExitCode, engine.ExitMiss)
	}
	if len(m.Writes()) != 0 || content(t, m, "a") != "old\n" {
		t.Errorf("the first file was touched: writes=%v", m.Writes())
	}
	if got := statusOf(rep); !reflect.DeepEqual(got, []string{engine.StatusNotWritten, engine.StatusFailed}) {
		t.Errorf("statuses = %v", got)
	}
}

func TestUnreadableFileIsAnIOErrorAndWritesNothing(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old\n"), 0o644)
	rep := run(t, m, []string{"a", "missing"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitIO || len(m.Writes()) != 0 {
		t.Fatalf("exit=%d writes=%v", rep.ExitCode, m.Writes())
	}
	if got := rep.Files[1].Error; !strings.Contains(got, "does not exist") || strings.Contains(got, "missing") {
		t.Errorf("error = %q: want the reason without the path, which the report prints itself", got)
	}
	// An I/O error outranks an anchor miss in the exit code.
	m.Put("b", []byte("no anchor\n"), 0o644)
	rep = run(t, m, []string{"b", "missing"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitIO {
		t.Errorf("exit = %d, want %d", rep.ExitCode, engine.ExitIO)
	}
}

func TestRefusesBinaryFiles(t *testing.T) {
	m := memfs.New()
	m.Put("bin", []byte("old\x00\x01"), 0o644)
	m.Put("txt", []byte("old\n"), 0o644)
	rep := run(t, m, []string{"txt", "bin"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitIO || len(m.Writes()) != 0 {
		t.Fatalf("exit=%d writes=%v", rep.ExitCode, m.Writes())
	}
	if !strings.Contains(rep.Files[1].Error, "binary") {
		t.Errorf("error = %q", rep.Files[1].Error)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old\n"), 0o644)
	rep := run(t, m, []string{"a"}, swap, engine.Options{DryRun: true})
	if rep.ExitCode != engine.ExitApplied || len(m.Writes()) != 0 || content(t, m, "a") != "old\n" {
		t.Fatalf("exit=%d writes=%v", rep.ExitCode, m.Writes())
	}
	f := rep.Files[0]
	if f.Status != engine.StatusWouldApply || f.Written || !f.Changed || !strings.Contains(f.Diff, "+new") {
		t.Errorf("file report: %+v", f)
	}
	// A dry run that would change nothing is "nothing to do", like a real run.
	rep = run(t, m, []string{"a"}, "=== replace\nold\n=== with\nold\n=== end\n", engine.Options{DryRun: true})
	if rep.ExitCode != engine.ExitNothing {
		t.Errorf("exit = %d, want %d", rep.ExitCode, engine.ExitNothing)
	}
}

func TestNothingToDo(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("new\n"), 0o644)
	rep := run(t, m, []string{"a"}, swap, engine.Options{Idempotent: true})
	if rep.ExitCode != engine.ExitNothing || len(m.Writes()) != 0 {
		t.Fatalf("exit=%d writes=%v: %+v", rep.ExitCode, m.Writes(), rep.Files)
	}
	if rep.Files[0].Status != engine.StatusUnchanged || rep.Files[0].Hunks[0].Status != "already-applied" {
		t.Errorf("file report: %+v", rep.Files[0])
	}
	// The same run without --idempotent is an anchor miss.
	rep = run(t, m, []string{"a"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitMiss {
		t.Errorf("exit = %d, want %d", rep.ExitCode, engine.ExitMiss)
	}
}

func TestSomeFilesAlreadyDoneIsStillApplied(t *testing.T) {
	m := memfs.New()
	m.Put("done", []byte("new\n"), 0o644)
	m.Put("todo", []byte("old\n"), 0o644)
	rep := run(t, m, []string{"done", "todo"}, swap, engine.Options{Idempotent: true})
	if rep.ExitCode != engine.ExitApplied {
		t.Fatalf("exit = %d: %+v", rep.ExitCode, rep.Files)
	}
	if got := statusOf(rep); !reflect.DeepEqual(got, []string{engine.StatusUnchanged, engine.StatusApplied}) {
		t.Errorf("statuses = %v", got)
	}
	if !reflect.DeepEqual(m.Writes(), []string{"todo"}) {
		t.Errorf("writes = %v: unchanged files must not be rewritten", m.Writes())
	}
}

func TestBackup(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old\n"), 0o600)
	rep := run(t, m, []string{"a"}, swap, engine.Options{Backup: true})
	if rep.ExitCode != engine.ExitApplied {
		t.Fatalf("exit = %d", rep.ExitCode)
	}
	if content(t, m, "a.bak") != "old\n" || content(t, m, "a") != "new\n" {
		t.Errorf("a.bak=%q a=%q", content(t, m, "a.bak"), content(t, m, "a"))
	}
	if rep.Files[0].Backup != "a.bak" {
		t.Errorf("Backup = %q", rep.Files[0].Backup)
	}
	if _, mode, _ := m.Get("a.bak"); mode != 0o600 {
		t.Errorf("backup mode = %o", mode)
	}
	// A dry run makes no backup.
	m2 := memfs.New()
	m2.Put("a", []byte("old\n"), 0o600)
	run(t, m2, []string{"a"}, swap, engine.Options{Backup: true, DryRun: true})
	if _, _, ok := m2.Get("a.bak"); ok {
		t.Error("a dry run created a backup")
	}
}

func TestWriteFailureRollsBackEarlierFiles(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old a\n"), 0o644)
	m.Put("b", []byte("old b\n"), 0o644)
	m.Put("c", []byte("old c\n"), 0o644)
	m.FailWrites("b", errors.New("disk full"))
	rep := run(t, m, []string{"a", "b", "c"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitIO {
		t.Fatalf("exit = %d", rep.ExitCode)
	}
	for _, name := range []string{"a", "b", "c"} {
		if got := content(t, m, name); got != "old "+name+"\n" {
			t.Errorf("%s = %q, want the original", name, got)
		}
	}
	want := []string{engine.StatusRolledBack, engine.StatusFailed, engine.StatusNotWritten}
	if got := statusOf(rep); !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
	if rep.Files[0].Written {
		t.Error("a rolled-back file must not be reported as written")
	}
	if len(rep.Notes) == 0 || !strings.Contains(rep.Notes[0], "restored") {
		t.Errorf("notes = %v", rep.Notes)
	}
}

// flaky fails the n-th write to a path (counting from 1).
type flaky struct {
	*memfs.FS
	path   string
	failOn int
	calls  int
}

func (f *flaky) Write(ctx context.Context, p string, data []byte, o fsys.WriteOptions) error {
	if p == f.path {
		f.calls++
		if f.calls == f.failOn {
			return errors.New("connection lost")
		}
	}
	return f.FS.Write(ctx, p, data, o)
}

func TestFailedRollbackIsReported(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old a\n"), 0o644)
	m.Put("b", []byte("old b\n"), 0o644)
	m.FailWrites("b", errors.New("disk full"))
	f := &flaky{FS: m, path: "a", failOn: 2} // the restore of a fails
	rep := run(t, f, []string{"a", "b"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitIO {
		t.Fatalf("exit = %d", rep.ExitCode)
	}
	if rep.Files[0].Status != engine.StatusLeftModified {
		t.Errorf("a status = %s, want %s", rep.Files[0].Status, engine.StatusLeftModified)
	}
	if got := content(t, m, "a"); got != "new a\n" {
		t.Errorf("a = %q", got)
	}
	if len(rep.Notes) == 0 || !strings.Contains(rep.Notes[0], "NOT be restored") {
		t.Errorf("notes = %v", rep.Notes)
	}
}

func TestConcurrentChangeIsAConflictAndRollsBack(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old a\n"), 0o644)
	m.Put("b", []byte("old b\n"), 0o644)
	m.AfterRead = func(p string) {
		if p == "b" {
			m.Put("b", []byte("someone else was here old\n"), 0o644)
		}
	}
	rep := run(t, m, []string{"a", "b"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitIO {
		t.Fatalf("exit = %d", rep.ExitCode)
	}
	if content(t, m, "b") != "someone else was here old\n" {
		t.Errorf("the concurrent change was overwritten: %q", content(t, m, "b"))
	}
	if content(t, m, "a") != "old a\n" {
		t.Errorf("a was not rolled back: %q", content(t, m, "a"))
	}
	if !strings.Contains(rep.Files[1].Error, "another process") {
		t.Errorf("error = %q", rep.Files[1].Error)
	}
}

func TestScopedSpec(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old a\n"), 0o644)
	m.Put("b", []byte("old b\n"), 0o644)
	m.Put("c", []byte("untouched old\n"), 0o644)
	var read []string
	m.AfterRead = func(p string) { read = append(read, p) }
	specText := "=== file a\n=== replace\nold a\n=== with\nA\n=== end\n=== file b\n=== append\nend of b\n=== end\n"
	rep := run(t, m, []string{"a", "b", "c"}, specText, engine.Options{})
	if rep.ExitCode != engine.ExitApplied {
		t.Fatalf("exit = %d: %+v", rep.ExitCode, rep.Files)
	}
	if content(t, m, "a") != "A\n" || content(t, m, "b") != "old b\nend of b\n" || content(t, m, "c") != "untouched old\n" {
		t.Errorf("contents: %q %q %q", content(t, m, "a"), content(t, m, "b"), content(t, m, "c"))
	}
	if !reflect.DeepEqual(read, []string{"a", "b"}) {
		t.Errorf("read %v: a file without hunks must not even be read", read)
	}
	if rep.Files[2].Status != engine.StatusUnchanged {
		t.Errorf("c status = %s", rep.Files[2].Status)
	}
}

func TestScopedFailureInOneFileStopsAll(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old a\n"), 0o644)
	m.Put("b", []byte("b\n"), 0o644)
	specText := "=== file a\n=== replace\nold a\n=== with\nA\n=== end\n=== file b\n=== replace\nnope\n=== with\nB\n=== end\n"
	rep := run(t, m, []string{"a", "b"}, specText, engine.Options{})
	if rep.ExitCode != engine.ExitMiss || len(m.Writes()) != 0 {
		t.Fatalf("exit=%d writes=%v", rep.ExitCode, m.Writes())
	}
}

func TestSymlinkedFileKeepsItsLink(t *testing.T) {
	m := memfs.New()
	m.Put("real", []byte("old\n"), 0o644)
	m.Symlink("link", "real")
	rep := run(t, m, []string{"link"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitApplied {
		t.Fatalf("exit = %d", rep.ExitCode)
	}
	if !m.IsSymlink("link") || content(t, m, "real") != "new\n" {
		t.Errorf("symlink=%v real=%q", m.IsSymlink("link"), content(t, m, "real"))
	}
}

func TestCRLFAndNoFinalNewlineSurvive(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("x\r\nold\r\ny"), 0o644)
	rep := run(t, m, []string{"a"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitApplied {
		t.Fatalf("exit = %d", rep.ExitCode)
	}
	if got := content(t, m, "a"); got != "x\r\nnew\r\ny" {
		t.Errorf("content = %q", got)
	}
	if !strings.Contains(rep.Files[0].Diff, "+new") || strings.Contains(rep.Files[0].Diff, "\r") {
		t.Errorf("the diff of a CRLF file must show LF text: %q", rep.Files[0].Diff)
	}
	if rep.Files[0].LineEnding != "crlf" {
		t.Errorf("LineEnding = %q", rep.Files[0].LineEnding)
	}
}

func TestCancelledContextIsAnIOError(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old\n"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sp, _ := spec.Parse(swap, spec.Options{})
	rep := engine.Run(ctx, m, engine.Request{Files: []string{"a"}, Spec: sp})
	if rep.ExitCode != engine.ExitIO || len(m.Writes()) != 0 {
		t.Fatalf("exit=%d writes=%v", rep.ExitCode, m.Writes())
	}
	if !strings.Contains(rep.Files[0].Error, "canceled") {
		t.Errorf("error = %q", rep.Files[0].Error)
	}
}

// lossy loses the connection on the first write to one path: after the write
// has taken effect (the reply was lost), or before it (drop).
type lossy struct {
	*memfs.FS
	path  string
	drop  bool
	calls int
}

func (l *lossy) Write(ctx context.Context, p string, data []byte, o fsys.WriteOptions) error {
	if p == l.path {
		l.calls++
		if l.calls == 1 {
			if !l.drop {
				if err := l.FS.Write(ctx, p, data, o); err != nil {
					return err
				}
			}
			return fmt.Errorf("%s: %w", p, fsys.ErrConnection)
		}
	}
	return l.FS.Write(ctx, p, data, o)
}

func TestLostConnectionDuringAWriteThatWentThroughIsUndone(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old a\n"), 0o644)
	m.Put("b", []byte("old b\n"), 0o644)
	rep := run(t, &lossy{FS: m, path: "b"}, []string{"a", "b"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitIO {
		t.Fatalf("exit = %d", rep.ExitCode)
	}
	for _, name := range []string{"a", "b"} {
		if got := content(t, m, name); got != "old "+name+"\n" {
			t.Errorf("%s = %q, want the original (the write whose reply was lost must be undone too)", name, got)
		}
	}
	if got := statusOf(rep); !reflect.DeepEqual(got, []string{engine.StatusRolledBack, engine.StatusRolledBack}) {
		t.Errorf("statuses = %v", got)
	}
	if !strings.Contains(strings.Join(rep.Notes, "|"), "holds the patched content") {
		t.Errorf("notes = %v", rep.Notes)
	}
}

func TestLostConnectionBeforeTheWriteLeavesTheFileAlone(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old a\n"), 0o644)
	m.Put("b", []byte("old b\n"), 0o644)
	rep := run(t, &lossy{FS: m, path: "b", drop: true}, []string{"a", "b"}, swap, engine.Options{})
	if rep.ExitCode != engine.ExitIO || content(t, m, "a") != "old a\n" || content(t, m, "b") != "old b\n" {
		t.Fatalf("exit=%d a=%q b=%q", rep.ExitCode, content(t, m, "a"), content(t, m, "b"))
	}
	if rep.Files[1].Status != engine.StatusFailed || !strings.Contains(strings.Join(rep.Notes, "|"), "was not replaced") {
		t.Errorf("b status=%s notes=%v", rep.Files[1].Status, rep.Notes)
	}
}

func TestNoDiffSkipsRenderingDiffs(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("old\n"), 0o644)
	rep := run(t, m, []string{"a"}, swap, engine.Options{NoDiff: true})
	if rep.ExitCode != engine.ExitApplied || !rep.Files[0].Changed || rep.Files[0].Diff != "" {
		t.Errorf("exit=%d changed=%v diff=%q", rep.ExitCode, rep.Files[0].Changed, rep.Files[0].Diff)
	}
}
