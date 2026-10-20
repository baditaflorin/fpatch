package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binary string

func TestMain(m *testing.M) {
	code := 1
	defer func() { os.Exit(code) }()
	dir, err := os.MkdirTemp("", "fpatch-e2e")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	binary = filepath.Join(dir, "fpatch")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=0.0.0-e2e", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		panic("go build failed: " + err.Error() + "\n" + string(out))
	}
	code = m.Run()
}

type out struct {
	code           int
	stdout, stderr string
}

func fpatch(t *testing.T, stdin string, env []string, args ...string) out {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out{code, o.String(), e.String()}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	d, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(d)
}

const swap = "=== replace\nold\n=== with\nnew\n=== end\n"

func TestBinaryVersion(t *testing.T) {
	if o := fpatch(t, "", nil, "--version"); o.code != 0 || o.stdout != "0.0.0-e2e\n" {
		t.Errorf("exit=%d stdout=%q", o.code, o.stdout)
	}
}

func TestBinaryAppliesASpecFromStdin(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "f", "a\nold\nb\n")
	o := fpatch(t, swap, nil, f)
	if o.code != 0 || readFile(t, f) != "a\nnew\nb\n" {
		t.Fatalf("exit=%d file=%q stderr=%q", o.code, readFile(t, f), o.stderr)
	}
	if !strings.Contains(o.stdout, "-old") || !strings.Contains(o.stdout, "+new") {
		t.Errorf("stdout lacks the diff: %q", o.stdout)
	}
}

func TestBinaryExitCodes(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "f", "old\n")
	cases := []struct {
		name  string
		stdin string
		args  []string
		code  int
	}{
		{"nothing to do", swap, []string{"--idempotent", writeFile(t, dir, "done", "new\n")}, 4},
		{"anchor missing", swap, []string{writeFile(t, dir, "other", "something else\n")}, 1},
		{"usage", swap, nil, 2},
		{"empty spec", "", []string{f}, 2},
		{"missing file", swap, []string{filepath.Join(dir, "absent")}, 3},
		{"applied", swap, []string{f}, 0},
	}
	for _, tc := range cases {
		if o := fpatch(t, tc.stdin, nil, tc.args...); o.code != tc.code {
			t.Errorf("%s: exit = %d, want %d\nstdout: %s\nstderr: %s", tc.name, o.code, tc.code, o.stdout, o.stderr)
		}
	}
}

func TestBinaryJSON(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "f", "old\n")
	o := fpatch(t, swap, nil, "--json", "--dry-run", f)
	if o.code != 0 {
		t.Fatalf("exit = %d: %s", o.code, o.stderr)
	}
	lines := strings.Split(strings.TrimSpace(o.stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q", o.stdout)
	}
	var file, sum map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &file); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &sum); err != nil {
		t.Fatal(err)
	}
	if file["status"] != "would-apply" || sum["type"] != "summary" {
		t.Errorf("file=%v summary=%v", file, sum)
	}
	if readFile(t, f) != "old\n" {
		t.Error("a dry run changed the file")
	}
}

// TestBinaryHostFlag runs --host against a stand-in ssh that executes the
// remote command locally, so the whole path (flag, factory, ssh process, remote
// helper, atomic write) is covered without a network.
func TestBinaryHostFlag(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case $1 in -o) shift 2;; --) shift; break;; -*) shift;; *) break;; esac; done\nshift\nexec /bin/sh -c \"$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	dir := t.TempDir()
	a := writeFile(t, dir, "a", "old a\n")
	b := writeFile(t, dir, "b", "old b\n")
	spec := "=== replace count=1\nold\n=== with\nnew\n=== end\n"
	o := fpatch(t, spec, env, "--host", "web1", "--backup", a, b)
	if o.code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", o.code, o.stdout, o.stderr)
	}
	if readFile(t, a) != "new a\n" || readFile(t, b) != "new b\n" || readFile(t, a+".bak") != "old a\n" {
		t.Errorf("a=%q b=%q a.bak=%q", readFile(t, a), readFile(t, b), readFile(t, a+".bak"))
	}
	// All-or-nothing holds over ssh as well.
	c := writeFile(t, dir, "c", "old c\n")
	d := writeFile(t, dir, "d", "nothing to match\n")
	o = fpatch(t, spec, env, "--host", "web1", c, d)
	if o.code != 1 || readFile(t, c) != "old c\n" {
		t.Errorf("exit=%d c=%q stderr=%q", o.code, readFile(t, c), o.stderr)
	}
	if !strings.Contains(o.stderr, "web1:"+d) {
		t.Errorf("messages about a remote file must name the host: %q", o.stderr)
	}
}

func TestBinaryRefusesAHostileHostAlias(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "f", "old\n")
	canary := filepath.Join(dir, "canary")
	o := fpatch(t, swap, nil, "--host", "-oProxyCommand=touch "+canary, f)
	if o.code != 2 {
		t.Errorf("exit = %d, want 2", o.code)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("the option was handed to ssh")
	}
}
