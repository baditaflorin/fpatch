package fsys_test

import (
	"context"
	"errors"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/fsys/fsystest"
)

// fakeSSH puts a stand-in for the ssh binary first in PATH. Like ssh it takes
// options, "--", a host and a remote command, and runs that command through
// "sh -c" the way a remote login would, but on this machine. That exercises
// the real wire protocol, the real remote helper and real coreutils without a
// network. Special host names misbehave on purpose. The returned path is a
// log with one line per ssh process started.
func fakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_SSH_LOG"
while [ $# -gt 0 ]; do
  case $1 in
    -o) shift 2 ;;
    --) shift; break ;;
    -*) shift ;;
    *) break ;;
  esac
done
host=$1
shift
case $host in
  unreachable*)
    echo "ssh: Could not resolve hostname $host: Name or service not known" >&2
    exit 255 ;;
  denied*)
    echo "$host: Permission denied (publickey)." >&2
    exit 255 ;;
  hang*)
    exec sleep 60 ;;
  nowrite*)
    # The remote shell never gets to run fp_write: the request line is
    # renamed to a command that does not exist.
    sed -u 's/^fp_write /fp_nowrite /' | /bin/sh -c "$*"
    exit $? ;;
  noisy*)
    printf 'Last login: yesterday, no newline'
    echo
    echo "Welcome. FPATCH lookalike: 00000000000000000000000000000000 hello"
    ;;
esac
exec /bin/sh -c "$*"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SSH_LOG", log)
	return log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func newSSH(t *testing.T, host string) fsys.FS {
	t.Helper()
	f, err := fsys.New("ssh:" + host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestSSHContract(t *testing.T) {
	fakeSSH(t)
	fsystest.Run(t, func(t *testing.T) (fsys.FS, fsystest.Env) {
		return newSSH(t, "web1"), fsystest.NewDirEnv(t)
	})
}

func TestSSHContractThroughALoudLogin(t *testing.T) {
	fakeSSH(t)
	env := fsystest.NewDirEnv(t)
	f := newSSH(t, "noisy-host")
	env.Put(t, "a", []byte("old\n"), 0o644)
	got, err := f.Read(context.Background(), env.Path("a"))
	if err != nil || string(got.Data) != "old\n" {
		t.Fatalf("Read = %q, %v", got.Data, err)
	}
	if err := f.Write(context.Background(), env.Path("a"), []byte("new\n"), fsys.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := env.Get(t, "a"); string(d) != "new\n" {
		t.Errorf("stored %q", d)
	}
}

func TestSSHUsesOneConnectionAndSafeOptions(t *testing.T) {
	log := fakeSSH(t)
	env := fsystest.NewDirEnv(t)
	f := newSSH(t, "web1")
	for i := 0; i < 5; i++ {
		name := "f" + string(rune('a'+i))
		env.Put(t, name, []byte("old\n"), 0o644)
		if _, err := f.Read(context.Background(), env.Path(name)); err != nil {
			t.Fatal(err)
		}
		if err := f.Write(context.Background(), env.Path(name), []byte("new\n"), fsys.WriteOptions{BackupSuffix: ".bak"}); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	lines := calls(t, log)
	if len(lines) != 1 {
		t.Fatalf("%d ssh processes were started for one run, want 1:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	line := lines[0]
	for _, want := range []string{"-T", "BatchMode=yes", "ClearAllForwardings=yes", "RemoteCommand=none", "ConnectTimeout=", "ServerAliveInterval="} {
		if !strings.Contains(line, want) {
			t.Errorf("ssh was called without %q: %s", want, line)
		}
	}
	if !strings.HasSuffix(line, " -- web1 sh") {
		t.Errorf("the host must come after \"--\" and the remote command must be a bare sh: %s", line)
	}
}

func TestSSHDoesNotConnectUntilNeeded(t *testing.T) {
	log := fakeSSH(t)
	f := newSSH(t, "web1")
	if _, err := f.Read(context.Background(), "bad\npath"); !errors.Is(err, fsys.ErrInvalidPath) {
		t.Fatalf("err = %v", err)
	}
	f.Close()
	if n := len(calls(t, log)); n != 0 {
		t.Errorf("an invalid path started %d ssh processes", n)
	}
}

func TestSSHConnectionFailureCarriesSSHsMessage(t *testing.T) {
	for host, want := range map[string]string{
		"unreachable-1": "Could not resolve hostname",
		"denied-1":      "Permission denied",
	} {
		t.Run(host, func(t *testing.T) {
			log := fakeSSH(t)
			f := newSSH(t, host)
			_, err := f.Read(context.Background(), "/etc/hostname")
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want it to mention %q", err, want)
			}
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				t.Errorf("a connection failure must not look like a file error: %v", err)
			}
			// The failure is sticky and does not retry in a loop.
			if err2 := f.Write(context.Background(), "/etc/hostname", []byte("x"), fsys.WriteOptions{}); err2 == nil {
				t.Error("Write after a failed connection succeeded")
			}
			if n := len(calls(t, log)); n != 1 {
				t.Errorf("%d ssh processes were started, want 1", n)
			}
		})
	}
}

func TestSSHContextCancelKillsTheConnection(t *testing.T) {
	fakeSSH(t)
	f := newSSH(t, "hang-1")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.Read(ctx, "/etc/hostname")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("Read took %v after a 300ms deadline", time.Since(start))
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestSSHRemoteWriteFailureIsReportedAndCleanedUp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	fakeSSH(t)
	env := fsystest.NewDirEnv(t)
	f := newSSH(t, "web1")
	env.Put(t, "ro/file", []byte("old\n"), 0o644)
	if err := os.Chmod(env.Path("ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(env.Path("ro"), 0o755) })
	err := f.Write(context.Background(), env.Path("ro/file"), []byte("new\n"), fsys.WriteOptions{})
	if err == nil || !strings.Contains(err.Error(), "temporary file") {
		t.Fatalf("err = %v, want a message about the temporary file", err)
	}
	if d, _, _ := env.Get(t, "ro/file"); string(d) != "old\n" {
		t.Errorf("the file changed: %q", d)
	}
	// The session survives a failed request.
	if got, err := f.Read(context.Background(), env.Path("ro/file")); err != nil || string(got.Data) != "old\n" {
		t.Errorf("Read after a failed Write: %q, %v", got.Data, err)
	}
}

func TestNewSSHRejectsHostileAliases(t *testing.T) {
	for _, alias := range []string{
		"", "-oProxyCommand=touch /tmp/pwned", "-", "-v", "a b", "a;b", "a|b", "a&b", "$(id)", "`id`", "a\nb", "a'b", `a"b`,
		"a\\b", "a*", "a?", "a<b", "a>b", "a b c", ":leading", "~x", "[::1]",
	} {
		if _, err := fsys.New("ssh:" + alias); err == nil {
			t.Errorf("alias %q was accepted", alias)
		}
	}
	for _, alias := range []string{"web1", "web-1", "web_1", "my_host-2.example.org", "deploy@web1", "203.0.113.7", "a", "0"} {
		f, err := fsys.New("ssh:" + alias)
		if err != nil {
			t.Errorf("alias %q was refused: %v", alias, err)
			continue
		}
		f.Close()
	}
}

func TestSSHTargetNeedsAHost(t *testing.T) {
	if _, err := fsys.New("ssh"); err == nil {
		t.Error("ssh without a host was accepted")
	}
}

func TestShQuoteSurvivesARealShell(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	const alphabet = "ab 'x\"\\$`;|&*?[]{}()<>!~#\n\t-=%é日"
	runes := []rune(alphabet)
	var inputs []string
	for i := 0; i < 600; i++ {
		n := r.Intn(14)
		var b strings.Builder
		for j := 0; j < n; j++ {
			b.WriteRune(runes[r.Intn(len(runes))])
		}
		inputs = append(inputs, b.String())
	}
	inputs = append(inputs, "", "'", "''", "'\\''", "$(touch pwned)", "`touch pwned`", "\\", "-n", "--")
	script := "printf '%s\\0'"
	for _, in := range inputs {
		script += " " + fsys.ShQuote(in)
	}
	dir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(got) != len(inputs) {
		t.Fatalf("got %d words back, want %d", len(got), len(inputs))
	}
	for i := range inputs {
		if got[i] != inputs[i] {
			t.Fatalf("input %d: %q came back as %q", i, inputs[i], got[i])
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("quoting let a command run: %v", entries)
	}
}

func FuzzShQuote(f *testing.F) {
	for _, s := range []string{"", "a", "'", "a b", "$(id)", "x\ny", "é", `\`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if strings.ContainsRune(s, 0) {
			return
		}
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+fsys.ShQuote(s)).Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(out) != s {
			t.Fatalf("%q came back as %q", s, out)
		}
	})
}

func TestPosixCksumMatchesTheCommand(t *testing.T) {
	if got := fsys.SSHVersion([]byte("123456789")); got != "930766865 9" {
		t.Errorf("version of the standard test vector = %q, want \"930766865 9\"", got)
	}
	path, err := exec.LookPath("cksum")
	if err != nil {
		t.Skip("no cksum command")
	}
	r := rand.New(rand.NewSource(11))
	for _, n := range []int{0, 1, 2, 255, 256, 257, 65535, 65536, 100000} {
		data := make([]byte, n)
		r.Read(data)
		cmd := exec.Command(path)
		cmd.Stdin = strings.NewReader(string(data))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fsys.SSHVersion(data), strings.Join(strings.Fields(string(out)), " "); got != want {
			t.Errorf("%d bytes: version %q, cksum says %q", n, got, want)
		}
	}
}

func TestHeredocDelimiterNeverOccursInTheData(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		d := fsys.HeredocDelimiter([]byte("FPATCH_EOF_\nFPATCH_EOF_0000000000000000\n"))
		if strings.Contains("FPATCH_EOF_\nFPATCH_EOF_0000000000000000\n", d) || seen[d] {
			t.Fatalf("bad delimiter %q", d)
		}
		seen[d] = true
	}
}

func TestSSHReconnectsOnceToLetARollbackRun(t *testing.T) {
	log := fakeSSH(t)
	env := fsystest.NewDirEnv(t)
	env.Put(t, "a", []byte("old\n"), 0o644)
	f := newSSH(t, "web1")
	ctx := context.Background()
	if _, err := f.Read(ctx, env.Path("a")); err != nil {
		t.Fatal(err)
	}
	fsys.KillSSHSession(f) // the connection drops between two requests
	if _, err := f.Read(ctx, env.Path("a")); !errors.Is(err, fsys.ErrConnection) {
		t.Fatalf("the request that finds the connection dead: %v, want fsys.ErrConnection", err)
	}
	got, err := f.Read(ctx, env.Path("a"))
	if err != nil || string(got.Data) != "old\n" {
		t.Fatalf("the next request must use a new session: %q, %v", got.Data, err)
	}
	if n := len(calls(t, log)); n != 2 {
		t.Errorf("%d ssh processes, want 2 (the original and one replacement)", n)
	}
	// Only one replacement: a second loss stays a failure.
	fsys.KillSSHSession(f)
	for i := 0; i < 2; i++ {
		if _, err := f.Read(ctx, env.Path("a")); !errors.Is(err, fsys.ErrConnection) {
			t.Fatalf("request %d after the second loss: %v, want fsys.ErrConnection", i, err)
		}
	}
	if n := len(calls(t, log)); n != 2 {
		t.Errorf("%d ssh processes, want still 2", n)
	}
}

func TestSSHReportsARequestTheRemoteShellCouldNotRun(t *testing.T) {
	if out, err := exec.Command("sed", "-u", "p").CombinedOutput(); err != nil {
		t.Skipf("sed -u is not available: %v %s", err, out)
	}
	fakeSSH(t)
	env := fsystest.NewDirEnv(t)
	env.Put(t, "a", []byte("old\n"), 0o644)
	f := newSSH(t, "nowrite-host")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := f.Write(ctx, env.Path("a"), []byte("new\n"), fsys.WriteOptions{})
	if err == nil || !strings.Contains(err.Error(), "without answering") {
		t.Fatalf("err = %v, want a report that the request got no answer (and not a hang)", err)
	}
	// The session is still in step afterwards.
	got, err := f.Read(ctx, env.Path("a"))
	if err != nil || string(got.Data) != "old\n" {
		t.Errorf("Read after the lost request: %q, %v", got.Data, err)
	}
}

// runHelper feeds a request to the embedded helper in a plain /bin/sh and
// returns what it printed.
func runHelper(t *testing.T, shell, request string) string {
	t.Helper()
	cmd := exec.Command(shell)
	cmd.Stdin = strings.NewReader("FPT=tok\n" + fsys.SSHPrelude + "\n" + request)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func TestHelperChecksTheStagedContentAgainstItsChecksum(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	q := fsys.ShQuote
	wrong := runHelper(t, "/bin/sh", "fp_write "+q(f)+" 4 '' '' '1 4' <<'D'\nnew\nD\n")
	if !strings.Contains(wrong, "tok err io transfer damaged") {
		t.Errorf("a wrong checksum was accepted: %q", wrong)
	}
	if d, _ := os.ReadFile(f); string(d) != "old\n" {
		t.Errorf("the file changed: %q", d)
	}
	right := runHelper(t, "/bin/sh", "fp_write "+q(f)+" 4 '' '' "+q(fsys.SSHVersion([]byte("new\n")))+" <<'D'\nnew\nD\n")
	if !strings.Contains(right, "tok done") {
		t.Errorf("the right checksum was refused: %q", right)
	}
	if d, _ := os.ReadFile(f); string(d) != "new\n" {
		t.Errorf("the file is %q", d)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

// bash runs a here-document that ends early (dash drops the command); the
// checksum must catch a cut-off body.
func TestHelperRefusesATruncatedHereDocumentUnderBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	q := fsys.ShQuote
	// Content "abcd" (4 bytes, no final newline), stream cut after "abc".
	out := runHelper(t, bash, "fp_write "+q(f)+" 4 '' '' "+q(fsys.SSHVersion([]byte("abcd")))+" <<'D'\nabc")
	if !strings.Contains(out, "transfer damaged") {
		t.Errorf("output = %q, want the damaged-transfer error", out)
	}
	if d, _ := os.ReadFile(f); string(d) != "old\n" {
		t.Errorf("the file changed: %q", d)
	}
}

func TestHelperModeKeepsSpecialBits(t *testing.T) {
	dir := t.TempDir()
	cases := map[os.FileMode]string{0o640: "0640", 0o755 | os.ModeSetuid: "4755", 0o777 | os.ModeSticky: "1777", 0: "0000", 0o4: "0004", 0o44: "0044"}
	for mode, want := range cases {
		f := filepath.Join(dir, "f"+want)
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f, mode); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(runHelper(t, "/bin/sh", "printf '%s' \"$(fp_mode "+fsys.ShQuote(f)+")\"\n")); got != want {
			t.Errorf("fp_mode of %v = %q, want %q", mode, got, want)
		}
	}
}
