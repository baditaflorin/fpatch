package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/fsys/memfs"
	"github.com/baditaflorin/fpatch/internal/cli"
)

// The tests drive the whole command line against in-memory file systems that
// are registered as the "mem" backend (--fs mem:NAME), which doubles as a
// test of the vendor-agnostic factory.
var (
	storesMu sync.Mutex
	stores   = map[string]*memfs.FS{}
)

func init() {
	fsys.Register("mem", func(arg string) (fsys.FS, error) {
		storesMu.Lock()
		defer storesMu.Unlock()
		m, ok := stores[arg]
		if !ok {
			return nil, fmt.Errorf("no in-memory store %q", arg)
		}
		return m, nil
	})
}

// newStore returns a store that --fs mem:NAME reaches, and NAME.
func newStore(t *testing.T) (*memfs.FS, string) {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	m := memfs.New()
	storesMu.Lock()
	stores[name] = m
	storesMu.Unlock()
	t.Cleanup(func() {
		storesMu.Lock()
		delete(stores, name)
		storesMu.Unlock()
	})
	return m, name
}

type result struct {
	code        int
	stdout, err string
}

func run(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run(args, cli.Env{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Version: "v9.9.9-test"})
	return result{code, out.String(), errb.String()}
}

func get(t *testing.T, m *memfs.FS, name string) string {
	t.Helper()
	d, _, ok := m.Get(name)
	if !ok {
		t.Fatalf("%s does not exist", name)
	}
	return string(d)
}

const swap = "=== replace\nold\n=== with\nnew\n=== end\n"

func TestFlagsMayAppearAnywhere(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	m.Put("-weird", []byte("old\n"), 0o644)
	for _, args := range [][]string{
		{"a", "--dry-run", "--fs", "mem:" + name},
		{"--fs", "mem:" + name, "-n", "a"},
		{"a", "--fs=mem:" + name, "--dry-run"},
		{"--fs", "mem:" + name, "--dry-run", "--", "a"},
		{"--fs", "mem:" + name, "--dry-run", "--", "-weird"},
		{"--fs", "mem:" + name, "--dry-run", "a", "--", "-weird"},
	} {
		r := run(t, swap, args...)
		if r.code != 0 || !strings.Contains(r.stdout, "dry run") {
			t.Errorf("%v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.err)
		}
	}
	if len(m.Writes()) != 0 {
		t.Errorf("a dry run wrote %v", m.Writes())
	}
}

func TestUsageErrors(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	mem := "mem:" + name
	cases := []struct {
		name  string
		stdin string
		args  []string
		want  string // in stderr
	}{
		{"no arguments", swap, nil, "no files"},
		{"no files", swap, []string{"--dry-run"}, "no files"},
		{"unknown flag", swap, []string{"--frobnicate", "a"}, "frobnicate"},
		{"flag without value", swap, []string{"a", "--fs"}, "argument"},
		{"empty host", swap, []string{"a", "--host", ""}, "--host needs a host alias"},
		{"empty host with equals", swap, []string{"a", "--host="}, "--host needs a host alias"},
		{"empty fs", swap, []string{"a", "--fs", ""}, "--fs needs a target"},
		{"host and fs", swap, []string{"a", "--host", "web1", "--fs", mem}, "cannot be used together"},
		{"unknown file system", swap, []string{"a", "--fs", "nosuch:x"}, `unknown file system "nosuch"`},
		{"hostile ssh alias", swap, []string{"a", "--host", "-oProxyCommand=touch pwned"}, "invalid ssh host alias"},
		{"ssh without a host", swap, []string{"a", "--fs", "ssh"}, "needs a host alias"},
		{"bad timeout", swap, []string{"a", "--timeout", "soon"}, "timeout"},
		{"empty spec", "", []string{"a", "--fs", mem}, "no hunks"},
		{"spec syntax error", "=== replace\nx\n", []string{"a", "--fs", mem}, "spec line 1"},
		{"text outside blocks", "oops\n", []string{"a", "--fs", mem}, "spec line 1"},
		{"scope names an unlisted file", "=== file b\n=== append\nx\n=== end\n", []string{"a", "--fs", mem}, "not one of the FILE arguments"},
		{"duplicate file", swap, []string{"a", "a", "--fs", mem}, "same file"},
		{"duplicate after cleaning", swap, []string{"a", "./a", "--fs", mem}, "same file"},
		{"control character in a path", swap, []string{"a\nb", "--fs", mem}, "control character"},
		{"empty path", swap, []string{"", "--fs", mem}, "empty path"},
		{"spec file missing", "", []string{"a", "--spec", filepath.Join(t.TempDir(), "nope"), "--fs", mem}, "cannot read the spec"},
		{"bad marker", swap, []string{"a", "--marker", "a b", "--fs", mem}, "marker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, tc.stdin, tc.args...)
			if r.code != 2 {
				t.Errorf("exit = %d, want 2\nstdout: %s\nstderr: %s", r.code, r.stdout, r.err)
			}
			if !strings.Contains(r.err, tc.want) {
				t.Errorf("stderr %q does not contain %q", r.err, tc.want)
			}
			if r.stdout != "" {
				t.Errorf("a usage error printed to stdout: %q", r.stdout)
			}
		})
	}
	if len(m.Writes()) != 0 {
		t.Errorf("usage errors wrote %v", m.Writes())
	}
}

func TestTerminalStdinIsNotRead(t *testing.T) {
	var out, errb bytes.Buffer
	code := cli.Run([]string{"a"}, cli.Env{Stdin: strings.NewReader(swap), Stdout: &out, Stderr: &errb, StdinIsTerminal: true})
	if code != 2 || !strings.Contains(errb.String(), "no spec on stdin") {
		t.Errorf("exit=%d stderr=%q", code, errb.String())
	}
}

func TestExitCodes(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	m.Put("done", []byte("new\n"), 0o644)
	mem := "--fs=mem:" + name
	cases := []struct {
		name  string
		files []string
		spec  string
		extra []string
		code  int
	}{
		{"applied", []string{"a"}, swap, nil, 0},
		{"anchor missing", []string{"done"}, swap, nil, 1},
		{"missing file", []string{"nope"}, swap, nil, 3},
		{"nothing to do", []string{"done"}, swap, []string{"--idempotent"}, 4},
		{"spec error", []string{"a"}, "garbage", nil, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m.Put("a", []byte("old\n"), 0o644)
			args := append(append([]string{mem}, tc.files...), tc.extra...)
			r := run(t, tc.spec, args...)
			if r.code != tc.code {
				t.Errorf("exit = %d, want %d\nstdout: %s\nstderr: %s", r.code, tc.code, r.stdout, r.err)
			}
		})
	}
}

func TestAmbiguousAnchorExitCode(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("x\nx\n"), 0o644)
	r := run(t, "=== replace\nx\n=== with\ny\n=== end\n", "--fs=mem:"+name, "a")
	if r.code != 1 {
		t.Fatalf("exit = %d, want 1", r.code)
	}
	if !strings.Contains(r.err, "matches 2 times") || !strings.Contains(r.err, "matches start at line 1, 2") {
		t.Errorf("stderr = %q", r.err)
	}
	if len(m.Writes()) != 0 {
		t.Errorf("wrote %v", m.Writes())
	}
}

func TestSuccessOutput(t *testing.T) {
	m, name := newStore(t)
	m.Put("conf.yml", []byte("a: 1\ntimeout: 30\nb: 2\n"), 0o644)
	r := run(t, "=== replace\ntimeout: 30\n=== with\ntimeout: 60\n=== end\n=== append\nc: 3\n=== end\n", "--fs=mem:"+name, "conf.yml")
	if r.code != 0 || r.err != "" {
		t.Fatalf("exit=%d stderr=%q", r.code, r.err)
	}
	for _, want := range []string{"--- conf.yml\n+++ conf.yml\n", "-timeout: 30\n", "+timeout: 60\n", "+c: 3\n", "fpatch: applied 2 hunks in 1 file"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if got := get(t, m, "conf.yml"); got != "a: 1\ntimeout: 60\nb: 2\nc: 3\n" {
		t.Errorf("file = %q", got)
	}
}

func TestQuietHidesTheDiff(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	r := run(t, swap, "--fs=mem:"+name, "-q", "a")
	if r.code != 0 || strings.Contains(r.stdout, "@@") || !strings.Contains(r.stdout, "applied 1 hunk in 1 file") {
		t.Errorf("exit=%d stdout=%q", r.code, r.stdout)
	}
}

func TestMissOutputShowsTheNearestMatch(t *testing.T) {
	m, name := newStore(t)
	m.Put("conf", []byte("server:\n  host: example.org\n  port: 8080\n  timeout: 30\nlogging:\n  level: info\n"), 0o644)
	r := run(t, "=== replace\n  host: example.org\n  port: 8081\n  timeout: 30\n=== with\nx\n=== end\n", "--fs=mem:"+name, "conf")
	if r.code != 1 {
		t.Fatalf("exit = %d", r.code)
	}
	for _, want := range []string{
		"conf: hunk 1 (replace, spec line 1): anchor not found",
		"nearest match: lines 2-4, similarity 0.9",
		"2 |   host: example.org",
		"3 |   port: 8080",
		"4 |   timeout: 30",
		"nothing was written",
	} {
		if !strings.Contains(r.err, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.err)
		}
	}
	if r.stdout != "" {
		t.Errorf("a failed run printed to stdout: %q", r.stdout)
	}
}

func TestMissOutputWhenNothingIsSimilar(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("alpha\nbeta\n"), 0o644)
	r := run(t, "=== replace\nzzzzzzzzzzzzzzzzzzzz qqqqqqqqqq\n=== with\nx\n=== end\n", "--fs=mem:"+name, "a")
	if r.code != 1 || !strings.Contains(r.err, "nothing in the file looks similar") {
		t.Errorf("exit=%d stderr=%q", r.code, r.err)
	}
}

func TestFailedRunPrintsNoDiffs(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	m.Put("b", []byte("no anchor\n"), 0o644)
	r := run(t, swap, "--fs=mem:"+name, "a", "b")
	if r.code != 1 || strings.Contains(r.stdout, "@@") || strings.Contains(r.err, "@@") {
		t.Errorf("exit=%d stdout=%q stderr=%q: a run that wrote nothing must not show diffs", r.code, r.stdout, r.err)
	}
}

func TestIOErrorsAreReportedOnceWithTheFileName(t *testing.T) {
	m, name := newStore(t)
	r := run(t, swap, "--fs=mem:"+name, "nope")
	if r.code != 3 || !strings.Contains(r.err, "fpatch: nope: file does not exist\n") {
		t.Errorf("exit=%d stderr=%q", r.code, r.err)
	}
	_ = m
}

func TestDryRunOutput(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	m.Put("b", []byte("old\n"), 0o644)
	r := run(t, swap, "--fs=mem:"+name, "--dry-run", "a", "b")
	if r.code != 0 || !strings.Contains(r.stdout, "dry run, nothing written: 2 hunks would change 2 files") {
		t.Errorf("exit=%d stdout=%q", r.code, r.stdout)
	}
	if strings.Count(r.stdout, "+new") != 2 {
		t.Errorf("a dry run must print every diff:\n%s", r.stdout)
	}
}

func TestIdempotentOutput(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("new\n"), 0o644)
	r := run(t, swap, "--fs=mem:"+name, "--idempotent", "a")
	if r.code != 4 || !strings.Contains(r.stdout, "already applied") || !strings.Contains(r.stdout, "nothing to do") {
		t.Errorf("exit=%d stdout=%q", r.code, r.stdout)
	}
}

func TestBackupFlag(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o640)
	r := run(t, swap, "--fs=mem:"+name, "--backup", "a")
	if r.code != 0 {
		t.Fatalf("exit=%d stderr=%q", r.code, r.err)
	}
	if get(t, m, "a.bak") != "old\n" || get(t, m, "a") != "new\n" {
		t.Errorf("a.bak=%q a=%q", get(t, m, "a.bak"), get(t, m, "a"))
	}
}

func TestSpecFromFileAndFromStdinDash(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	specFile := filepath.Join(t.TempDir(), "spec.txt")
	if err := os.WriteFile(specFile, []byte(swap), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := run(t, "ignored stdin", "--fs=mem:"+name, "--spec", specFile, "a"); r.code != 0 {
		t.Fatalf("--spec: exit=%d stderr=%q", r.code, r.err)
	}
	m.Put("a", []byte("old\n"), 0o644)
	if r := run(t, swap, "--fs=mem:"+name, "-f", "-", "a"); r.code != 0 {
		t.Fatalf("-f -: exit=%d stderr=%q", r.code, r.err)
	}
	if get(t, m, "a") != "new\n" {
		t.Errorf("a = %q", get(t, m, "a"))
	}
}

func TestMarkerFlag(t *testing.T) {
	m, name := newStore(t)
	m.Put("doc", []byte("see\n=== end\nabove\n"), 0o644)
	r := run(t, "@@ replace\n=== end\n@@ with\n=== stop\n@@ end\n", "--fs=mem:"+name, "--marker=@@", "doc")
	if r.code != 0 || get(t, m, "doc") != "see\n=== stop\nabove\n" {
		t.Errorf("exit=%d doc=%q stderr=%q", r.code, get(t, m, "doc"), r.err)
	}
}

func TestScopedSpecThroughTheCommandLine(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old a\n"), 0o644)
	m.Put("b", []byte("old b\n"), 0o644)
	r := run(t, "=== file a\n=== replace\nold a\n=== with\nA\n=== end\n=== file b\n=== replace\nold b\n=== with\nB\n=== end\n", "--fs=mem:"+name, "a", "b")
	if r.code != 0 || get(t, m, "a") != "A\n" || get(t, m, "b") != "B\n" {
		t.Errorf("exit=%d a=%q b=%q stderr=%q", r.code, get(t, m, "a"), get(t, m, "b"), r.err)
	}
}

func TestTimeoutIsReported(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	r := run(t, swap, "--fs=mem:"+name, "--timeout=1ns", "a")
	if r.code != 3 || !strings.Contains(r.err, "timeout") {
		t.Errorf("exit=%d stderr=%q", r.code, r.err)
	}
	if len(m.Writes()) != 0 {
		t.Errorf("wrote %v", m.Writes())
	}
}

func TestVersionAndHelp(t *testing.T) {
	if r := run(t, "", "--version"); r.code != 0 || r.stdout != "v9.9.9-test\n" {
		t.Errorf("--version: exit=%d stdout=%q", r.code, r.stdout)
	}
	for _, flag := range []string{"--help", "-h"} {
		r := run(t, "", flag)
		if r.code != 0 {
			t.Errorf("%s: exit = %d", flag, r.code)
		}
		for _, want := range []string{"Usage:", "=== replace", "=== upsert-section", "--idempotent", "Exit codes:", "4  nothing to do"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%s: help lacks %q", flag, want)
			}
		}
	}
}

// jsonLines decodes the NDJSON on stdout.
func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var all []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("stdout line is not JSON: %q\n%v", line, err)
		}
		all = append(all, obj)
	}
	return all
}

func TestJSONOutputSuccess(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("x\nold\ny\n"), 0o644)
	m.Put("b", []byte("done already: new\n"), 0o644)
	r := run(t, swap, "--fs=mem:"+name, "--json", "--idempotent", "a", "b")
	if r.code != 0 || r.err != "" {
		t.Fatalf("exit=%d stderr=%q", r.code, r.err)
	}
	objs := jsonLines(t, r.stdout)
	if len(objs) != 3 {
		t.Fatalf("%d objects, want 2 files and a summary:\n%s", len(objs), r.stdout)
	}
	a, b, sum := objs[0], objs[1], objs[2]
	if a["type"] != "file" || a["file"] != "a" || a["status"] != "applied" || a["changed"] != true || a["written"] != true {
		t.Errorf("file a: %v", a)
	}
	if !strings.Contains(a["diff"].(string), "+new") {
		t.Errorf("file a has no diff: %v", a["diff"])
	}
	hunks := a["hunks"].([]any)
	h := hunks[0].(map[string]any)
	for _, key := range []string{"index", "op", "spec_line", "status", "matches", "expected"} {
		if _, ok := h[key]; !ok {
			t.Errorf("hunk object lacks %q: %v", key, h)
		}
	}
	if h["op"] != "replace" || h["status"] != "applied" || h["matches"] != float64(1) || h["expected"] != float64(1) {
		t.Errorf("hunk: %v", h)
	}
	if b["status"] != "unchanged" || b["changed"] != false || b["written"] != false {
		t.Errorf("file b: %v", b)
	}
	if b["hunks"].([]any)[0].(map[string]any)["status"] != "already-applied" {
		t.Errorf("file b hunk: %v", b["hunks"])
	}
	if sum["type"] != "summary" || sum["exit_code"] != float64(0) || sum["files"] != float64(2) || sum["changed"] != float64(1) || sum["written"] != float64(1) || sum["dry_run"] != false {
		t.Errorf("summary: %v", sum)
	}
}

func TestJSONOutputFailure(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("server:\n  port: 8080\n"), 0o644)
	m.Put("b", []byte("old\n"), 0o644)
	r := run(t, "=== replace\n  port: 8081\n=== with\nx\n=== end\n", "--fs=mem:"+name, "--json", "b", "a")
	if r.code != 1 || r.err != "" {
		t.Fatalf("exit=%d stderr=%q (JSON mode keeps stderr empty)", r.code, r.err)
	}
	objs := jsonLines(t, r.stdout)
	if len(objs) != 3 {
		t.Fatalf("%d objects:\n%s", len(objs), r.stdout)
	}
	b, a, sum := objs[0], objs[1], objs[2]
	if b["status"] != "failed" || a["status"] != "failed" {
		t.Errorf("statuses: %v %v", b["status"], a["status"])
	}
	h := a["hunks"].([]any)[0].(map[string]any)
	if h["status"] != "missing" {
		t.Fatalf("hunk: %v", h)
	}
	near := h["nearest"].(map[string]any)
	if near["start_line"] != float64(2) || near["end_line"] != float64(2) || near["similarity"].(float64) < 0.8 {
		t.Errorf("nearest: %v", near)
	}
	if sum["exit_code"] != float64(1) || sum["written"] != float64(0) {
		t.Errorf("summary: %v", sum)
	}
	if len(m.Writes()) != 0 {
		t.Errorf("wrote %v", m.Writes())
	}
}

func TestJSONOutputForUsageErrors(t *testing.T) {
	r := run(t, swap, "--json", "a", "--fs", "nosuch")
	if r.code != 2 {
		t.Fatalf("exit = %d", r.code)
	}
	objs := jsonLines(t, r.stdout)
	if len(objs) != 1 || objs[0]["type"] != "summary" || objs[0]["exit_code"] != float64(2) || !strings.Contains(objs[0]["error"].(string), "nosuch") {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestQuietAlsoDropsTheDiffFromJSON(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("secret: old\n"), 0o644)
	r := run(t, swap, "--fs=mem:"+name, "--json", "--quiet", "a")
	objs := jsonLines(t, r.stdout)
	if r.code != 0 || objs[0]["changed"] != true {
		t.Fatalf("exit=%d stdout=%q", r.code, r.stdout)
	}
	if _, has := objs[0]["diff"]; has || strings.Contains(r.stdout, "secret") {
		t.Errorf("--quiet must keep file content out of the output: %q", r.stdout)
	}
}

func TestJSONDryRun(t *testing.T) {
	m, name := newStore(t)
	m.Put("a", []byte("old\n"), 0o644)
	r := run(t, swap, "--fs=mem:"+name, "--json", "-n", "a")
	objs := jsonLines(t, r.stdout)
	if r.code != 0 || objs[0]["status"] != "would-apply" || objs[0]["written"] != false || objs[1]["dry_run"] != true {
		t.Errorf("exit=%d stdout=%q", r.code, r.stdout)
	}
}

// The tests below use the real local file system.

func write(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	d, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(d)
}

func TestLocalEndToEnd(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.conf", "listen 80;\r\nserver_name old.example.org;\r\n", 0o640)
	b := write(t, dir, "b.conf", "server_name old.example.org;", 0o755)
	r := run(t, "=== replace count=1\nserver_name old.example.org;\n=== with\nserver_name new.example.org;\n=== end\n", a, b)
	if r.code != 0 {
		t.Fatalf("exit=%d stderr=%q", r.code, r.err)
	}
	if got := read(t, a); got != "listen 80;\r\nserver_name new.example.org;\r\n" {
		t.Errorf("a = %q", got)
	}
	if got := read(t, b); got != "server_name new.example.org;" {
		t.Errorf("b = %q (a missing final newline must stay missing)", got)
	}
	for p, want := range map[string]os.FileMode{a: 0o640, b: 0o755} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestLocalAllOrNothingAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a", "old\n", 0o644)
	b := write(t, dir, "b", "no anchor here\n", 0o644)
	r := run(t, swap, a, b)
	if r.code != 1 {
		t.Fatalf("exit = %d\n%s", r.code, r.err)
	}
	if read(t, a) != "old\n" || read(t, b) != "no anchor here\n" {
		t.Error("a file was modified although the run failed")
	}
	if !strings.Contains(r.err, filepath.Join(dir, "b")+": hunk 1") {
		t.Errorf("stderr should name the failing file:\n%s", r.err)
	}
}

func TestLocalSymlinkIsFollowed(t *testing.T) {
	dir := t.TempDir()
	real := write(t, dir, "real", "old\n", 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink("real", link); err != nil {
		t.Skip(err)
	}
	if r := run(t, swap, link); r.code != 0 {
		t.Fatalf("exit=%d %s", r.code, r.err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	if read(t, real) != "new\n" {
		t.Errorf("real = %q", read(t, real))
	}
}

func TestLocalBinaryFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	bin := write(t, dir, "bin", "old\x00\x01\x02", 0o644)
	r := run(t, swap, bin)
	if r.code != 3 || !strings.Contains(r.err, "binary file") {
		t.Errorf("exit=%d stderr=%q", r.code, r.err)
	}
	if read(t, bin) != "old\x00\x01\x02" {
		t.Error("the binary file was modified")
	}
}

func TestLocalMissingFileIsAnIOError(t *testing.T) {
	r := run(t, swap, filepath.Join(t.TempDir(), "nope"))
	if r.code != 3 || !strings.Contains(r.err, "no such file") {
		t.Errorf("exit=%d stderr=%q", r.code, r.err)
	}
}

func TestLocalUnwritableDirectoryRollsBackAndReports(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	okDir := filepath.Join(dir, "ok")
	roDir := filepath.Join(dir, "ro")
	for _, d := range []string{okDir, roDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a := write(t, okDir, "a", "old a\n", 0o644)
	b := write(t, roDir, "b", "old b\n", 0o644)
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(roDir, 0o755) })
	r := run(t, swap, a, b)
	if r.code != 3 {
		t.Fatalf("exit=%d stderr=%q", r.code, r.err)
	}
	if read(t, a) != "old a\n" || read(t, b) != "old b\n" {
		t.Errorf("files after a failed run: %q %q", read(t, a), read(t, b))
	}
	if !strings.Contains(r.err, "restored to its original content") {
		t.Errorf("stderr should report the rollback:\n%s", r.err)
	}
}
