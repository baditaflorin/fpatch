// Package fsystest is the contract test suite for fsys.FS implementations.
// Every backend, built in or not, passes it by calling Run from its tests:
//
//	func TestContract(t *testing.T) {
//		fsystest.Run(t, func(t *testing.T) (fsys.FS, fsystest.Env) {
//			env := fsystest.NewDirEnv(t)
//			return myfs.New(env.Root), env
//		})
//	}
//
// The suite needs a way to look at the backing store behind the FS's back
// (put a file there, read one back, make a symlink); that is the Env, which a
// backend supplies. NewDirEnv is the Env for backends whose store is a
// directory of the local machine, including ones that reach it through a
// different protocol.
package fsystest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/fpatch/fsys"
)

// Env gives the tests direct access to the store behind an FS. Names are
// relative to the test area; Path turns a name into the path the FS under
// test is given.
type Env interface {
	Path(name string) string
	// Rel returns a relative path for name, valid for the FS (a directory
	// based Env changes the working directory, so call it before the FS does
	// its first operation).
	Rel(t testing.TB, name string) string
	// Put creates or replaces a regular file, creating parent directories.
	Put(t testing.TB, name string, data []byte, mode fs.FileMode)
	Mkdir(t testing.TB, name string)
	// Symlink makes link a symlink to target (both names), creating parent
	// directories of link.
	Symlink(t testing.TB, link, target string)
	// Get reads the file behind name (following symlinks).
	Get(t testing.TB, name string) (data []byte, mode fs.FileMode, ok bool)
	IsSymlink(t testing.TB, name string) bool
	// Entries lists everything in the test area, sorted, as names.
	Entries(t testing.TB) []string
}

// Factory returns a fresh FS and the Env that sees its store.
type Factory func(t *testing.T) (fsys.FS, Env)

// Run executes the contract against the FS the factory returns.
func Run(t *testing.T, newFS Factory) {
	tests := []struct {
		name string
		fn   func(*testing.T, context.Context, fsys.FS, Env)
	}{
		{"ReadWriteRoundTrip", roundTrip},
		{"NamesNeedingCare", namesNeedingCare},
		{"RelativePaths", relativePaths},
		{"NotFound", notFound},
		{"DanglingSymlink", dangling},
		{"NotRegular", notRegular},
		{"ModePreserved", modePreserved},
		{"SymlinkFollowed", symlinkFollowed},
		{"NoTempFilesLeft", noTempFilesLeft},
		{"Conflict", conflict},
		{"Backup", backup},
		{"BackupDoesNotFollowSymlinks", backupSymlink},
		{"BackupFailureLeavesTheFileAlone", backupFailure},
		{"InvalidPaths", invalidPaths},
		{"CancelledContext", cancelled},
		{"ReadSeesExternalChanges", externalChanges},
		{"Concurrent", concurrent},
		{"CloseTwice", closeTwice},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, env := newFS(t)
			t.Cleanup(func() { f.Close() })
			// A backend that hangs (a remote shell waiting for a closing quote,
			// say) must fail the test, not stall the run until the test binary
			// times out.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			tc.fn(t, ctx, f, env)
		})
	}
}

func mustRead(t *testing.T, ctx context.Context, f fsys.FS, path string) fsys.File {
	t.Helper()
	got, err := f.Read(ctx, path)
	if err != nil {
		t.Fatalf("Read(%q): %v", path, err)
	}
	return got
}

func mustWrite(t *testing.T, ctx context.Context, f fsys.FS, path string, data []byte, o fsys.WriteOptions) {
	t.Helper()
	if err := f.Write(ctx, path, data, o); err != nil {
		t.Fatalf("Write(%q): %v", path, err)
	}
}

func mustGet(t *testing.T, env Env, name string) ([]byte, fs.FileMode) {
	t.Helper()
	d, m, ok := env.Get(t, name)
	if !ok {
		t.Fatalf("%s does not exist in the store", name)
	}
	return d, m
}

func added(before, after []string) []string {
	have := map[string]bool{}
	for _, b := range before {
		have[b] = true
	}
	var out []string
	for _, a := range after {
		if !have[a] {
			out = append(out, a)
		}
	}
	return out
}

func payloads() []struct {
	name string
	data []byte
} {
	var all []byte
	for r := 0; r < 4; r++ {
		for i := 1; i < 256; i++ {
			all = append(all, byte(i))
		}
	}
	return []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"one byte", []byte("a")},
		{"newline only", []byte("\n")},
		{"two newlines", []byte("\n\n")},
		{"no final newline", []byte("line1\nline2")},
		{"final newline", []byte("line1\nline2\n")},
		{"leading blank lines", []byte("\n\n\ntext")},
		{"crlf", []byte("a\r\nb\r\n")},
		{"lone cr", []byte("a\rb")},
		{"ends with cr", []byte("abc\r")},
		{"every byte but NUL", all},
		{"utf-8", []byte("héllo ☃ 日本語\n")},
		{"bom", []byte("\xef\xbb\xbfhello\n")},
		{"here-document lookalikes", []byte("EOF\nFPATCH_EOF\nFPATCH_EOF_0123456789abcdef\n'\n\"\n$HOME `id` $(id)\n\\\n\\n\n")},
		{"shell metacharacters", []byte("; rm -rf / #\n&& || | > < * ? [a] {b} ~ !\n")},
		{"reply lookalike", []byte("0123456789abcdef0123456789abcdef end 0\n\n0123456789abcdef0123456789abcdef ok 644\n")},
		{"long line", bytes.Repeat([]byte("x"), 1<<20)},
		{"many lines", bytes.Repeat([]byte("some line of text\n"), 60000)},
	}
}

func roundTrip(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	for _, p := range payloads() {
		env.Put(t, "rt-"+p.name, p.data, 0o644)
		got := mustRead(t, ctx, f, env.Path("rt-"+p.name))
		if !bytes.Equal(got.Data, p.data) {
			t.Fatalf("%s: Read returned %d bytes, want %d (first difference at %d)", p.name, len(got.Data), len(p.data), firstDiff(got.Data, p.data))
		}
		want := append([]byte("W"), p.data...)
		mustWrite(t, ctx, f, env.Path("rt-"+p.name), want, fsys.WriteOptions{})
		stored, _ := mustGet(t, env, "rt-"+p.name)
		if !bytes.Equal(stored, want) {
			t.Fatalf("%s: Write stored %d bytes, want %d (first difference at %d)", p.name, len(stored), len(want), firstDiff(stored, want))
		}
		again := mustRead(t, ctx, f, env.Path("rt-"+p.name))
		if !bytes.Equal(again.Data, want) {
			t.Fatalf("%s: Read after Write returned different data", p.name)
		}
		if got.Version != "" && got.Version == again.Version {
			t.Errorf("%s: the version did not change with the content", p.name)
		}
		// Writing the content a file already has is allowed.
		mustWrite(t, ctx, f, env.Path("rt-"+p.name), want, fsys.WriteOptions{})
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

var tricky = []string{
	"with space", "two  spaces", "single'quote", `double"quote`, "dollar$HOME", "back`tick`", "semi;colon",
	"amp&ersand", "pipe|pipe", "star*", "question?", "[bracket]", "{brace}", "paren(s)", "back\\slash",
	"percent%s", "bang!", "tilde~", "hash#", "ünïcode-日本語", "-leading-dash", "--", "-n", "ends-with-space ",
	" starts-with-space", "a'b\"c d$e",
	// If any of these were handed to a shell as code, it would create a file
	// called pwned-N in the working directory (which namesNeedingCare makes
	// the test area).
	"x; touch pwned-1", "$(touch pwned-2)", "`touch pwned-3`", "x' ; touch pwned-4 ; echo '",
	"x\" ; touch pwned-5 ; echo \"", "x && touch pwned-6", "x | touch pwned-7", "x > pwned-8", "x\\' ; touch pwned-9 ; echo '",
}

func namesNeedingCare(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Rel(t, "") // work in the test area, so a stray command would leave its mark there
	for i, name := range tricky {
		// Names go in below a directory, so that "~", "-" and friends are
		// exercised mid-path too; a leading "-" in a relative path is the
		// business of RelativePaths.
		rel := fmt.Sprintf("dir %d/%s", i, name)
		env.Put(t, rel, []byte("old\n"), 0o640)
		path := env.Path(rel)
		if got := mustRead(t, ctx, f, path); string(got.Data) != "old\n" {
			t.Fatalf("%q: Read = %q", name, got.Data)
		}
		mustWrite(t, ctx, f, path, []byte("new\n"), fsys.WriteOptions{BackupSuffix: ".bak"})
		if d, _ := mustGet(t, env, rel); string(d) != "new\n" {
			t.Fatalf("%q: stored %q", name, d)
		}
		if d, _ := mustGet(t, env, rel+".bak"); string(d) != "old\n" {
			t.Fatalf("%q: backup holds %q", name, d)
		}
	}
	for _, e := range env.Entries(t) {
		if strings.HasPrefix(e, "pwned") {
			t.Errorf("%q exists: a path was interpreted by a shell", e)
		}
	}
}

func relativePaths(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	for _, name := range []string{"plain", "-dash", "--", "-n", "~tilde", "!bang", "with space", "(paren)"} {
		rel := env.Rel(t, name)
		env.Put(t, name, []byte("old\n"), 0o644)
		if got := mustRead(t, ctx, f, rel); string(got.Data) != "old\n" {
			t.Fatalf("%q: Read = %q", rel, got.Data)
		}
		mustWrite(t, ctx, f, rel, []byte("new\n"), fsys.WriteOptions{BackupSuffix: ".bak"})
		if d, _ := mustGet(t, env, name); string(d) != "new\n" {
			t.Fatalf("%q: stored %q", rel, d)
		}
		if d, _ := mustGet(t, env, name+".bak"); string(d) != "old\n" {
			t.Fatalf("%q: backup holds %q", rel, d)
		}
	}
}

func notFound(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	before := env.Entries(t)
	p := env.Path("missing")
	if _, err := f.Read(ctx, p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read of a missing file: %v, want fs.ErrNotExist", err)
	}
	if err := f.Write(ctx, p, []byte("x"), fsys.WriteOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Write to a missing file: %v, want fs.ErrNotExist", err)
	}
	if err := f.Write(ctx, env.Path("no-dir/missing"), []byte("x"), fsys.WriteOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Write below a missing directory: %v, want fs.ErrNotExist", err)
	}
	if extra := added(before, env.Entries(t)); len(extra) != 0 {
		t.Errorf("Write created %v; it must never create files", extra)
	}
}

func dangling(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Symlink(t, "dangling", "nowhere")
	before := env.Entries(t)
	if _, err := f.Read(ctx, env.Path("dangling")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read through a dangling symlink: %v, want fs.ErrNotExist", err)
	}
	if err := f.Write(ctx, env.Path("dangling"), []byte("x"), fsys.WriteOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Write through a dangling symlink: %v, want fs.ErrNotExist", err)
	}
	if extra := added(before, env.Entries(t)); len(extra) != 0 {
		t.Errorf("Write created %v through a dangling symlink", extra)
	}
}

func notRegular(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Mkdir(t, "adir")
	if _, err := f.Read(ctx, env.Path("adir")); !errors.Is(err, fsys.ErrNotRegular) {
		t.Errorf("Read of a directory: %v, want fsys.ErrNotRegular", err)
	}
	if err := f.Write(ctx, env.Path("adir"), []byte("x"), fsys.WriteOptions{}); !errors.Is(err, fsys.ErrNotRegular) {
		t.Errorf("Write to a directory: %v, want fsys.ErrNotRegular", err)
	}
}

func modePreserved(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	for _, mode := range []fs.FileMode{0o600, 0o644, 0o755, 0o640, 0o444, 0o400, 0o700, 0o664, 0o750} {
		name := fmt.Sprintf("mode-%o", mode)
		env.Put(t, name, []byte("old\n"), mode)
		got := mustRead(t, ctx, f, env.Path(name))
		if got.Mode != 0 && got.Mode != mode {
			t.Errorf("%o: Read reports mode %o", mode, got.Mode)
		}
		mustWrite(t, ctx, f, env.Path(name), []byte("new\n"), fsys.WriteOptions{BackupSuffix: ".bak"})
		if _, m := mustGet(t, env, name); m != mode {
			t.Errorf("%o: mode after Write is %o", mode, m)
		}
		if _, m := mustGet(t, env, name+".bak"); m != mode {
			t.Errorf("%o: the backup has mode %o", mode, m)
		}
	}
}

func symlinkFollowed(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "target", []byte("old\n"), 0o600)
	env.Symlink(t, "link", "target")
	env.Symlink(t, "link2", "link")
	env.Put(t, "sub/real", []byte("sub old\n"), 0o644)
	env.Symlink(t, "other/link", "sub/real")
	for _, tc := range []struct{ link, target, old string }{
		{"link", "target", "old\n"},
		{"link2", "target", "new\n"},
		{"other/link", "sub/real", "sub old\n"},
	} {
		if got := mustRead(t, ctx, f, env.Path(tc.link)); string(got.Data) != tc.old {
			t.Fatalf("Read(%s) = %q, want %q", tc.link, got.Data, tc.old)
		}
		mustWrite(t, ctx, f, env.Path(tc.link), []byte("new\n"), fsys.WriteOptions{})
		if !env.IsSymlink(t, tc.link) {
			t.Errorf("%s is not a symlink any more", tc.link)
		}
		if d, _ := mustGet(t, env, tc.target); string(d) != "new\n" {
			t.Errorf("%s holds %q after writing through %s", tc.target, d, tc.link)
		}
	}
	if _, m := mustGet(t, env, "target"); m != 0o600 {
		t.Errorf("the target's mode changed to %o", m)
	}
	// A backup sits next to the name that was given, not next to the target.
	env.Put(t, "bt/target", []byte("old\n"), 0o644)
	env.Symlink(t, "bl/link", "bt/target")
	mustWrite(t, ctx, f, env.Path("bl/link"), []byte("new\n"), fsys.WriteOptions{BackupSuffix: ".bak"})
	if d, _ := mustGet(t, env, "bl/link.bak"); string(d) != "old\n" {
		t.Errorf("bl/link.bak = %q", d)
	}
	if _, _, ok := env.Get(t, "bt/target.bak"); ok {
		t.Error("the backup was put next to the symlink's target")
	}
}

func noTempFilesLeft(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "d/file", []byte("old\n"), 0o644)
	before := env.Entries(t)
	mustWrite(t, ctx, f, env.Path("d/file"), []byte("new\n"), fsys.WriteOptions{})
	if extra := added(before, env.Entries(t)); len(extra) != 0 {
		t.Errorf("a successful Write left %v behind", extra)
	}
	_ = f.Write(ctx, env.Path("d/file"), []byte("x"), fsys.WriteOptions{ExpectVersion: "999999 1"})
	_ = f.Write(ctx, env.Path("d/missing"), []byte("x"), fsys.WriteOptions{})
	if extra := added(before, env.Entries(t)); len(extra) != 0 {
		t.Errorf("failed Writes left %v behind", extra)
	}
}

func conflict(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "c", []byte("one\n"), 0o644)
	r := mustRead(t, ctx, f, env.Path("c"))
	if r.Version == "" {
		t.Skip("this backend has no versions, so it cannot detect concurrent changes")
	}
	// The version token is opaque, but a change of content must change it.
	env.Put(t, "c", []byte("two, changed behind our back\n"), 0o644)
	err := f.Write(ctx, env.Path("c"), []byte("ours\n"), fsys.WriteOptions{ExpectVersion: r.Version})
	if !errors.Is(err, fsys.ErrConflict) {
		t.Fatalf("Write after a concurrent change: %v, want fsys.ErrConflict", err)
	}
	if d, _ := mustGet(t, env, "c"); string(d) != "two, changed behind our back\n" {
		t.Errorf("a conflicting Write modified the file: %q", d)
	}
	r2 := mustRead(t, ctx, f, env.Path("c"))
	mustWrite(t, ctx, f, env.Path("c"), []byte("ours\n"), fsys.WriteOptions{ExpectVersion: r2.Version})
	if d, _ := mustGet(t, env, "c"); string(d) != "ours\n" {
		t.Errorf("a Write with the current version was refused or lost: %q", d)
	}
	// A conflicting Write makes no backup either.
	env.Put(t, "c2", []byte("one\n"), 0o644)
	r3 := mustRead(t, ctx, f, env.Path("c2"))
	env.Put(t, "c2", []byte("changed\n"), 0o644)
	_ = f.Write(ctx, env.Path("c2"), []byte("ours\n"), fsys.WriteOptions{ExpectVersion: r3.Version, BackupSuffix: ".bak"})
	if _, _, ok := env.Get(t, "c2.bak"); ok {
		t.Error("a conflicting Write left a backup")
	}
}

func backup(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "b", []byte("original\n"), 0o640)
	mustWrite(t, ctx, f, env.Path("b"), []byte("patched\n"), fsys.WriteOptions{BackupSuffix: ".orig"})
	if d, m := mustGet(t, env, "b.orig"); string(d) != "original\n" || m != 0o640 {
		t.Errorf("backup = %q mode %o", d, m)
	}
	if d, _ := mustGet(t, env, "b"); string(d) != "patched\n" {
		t.Errorf("file = %q", d)
	}
	// An existing backup is replaced.
	mustWrite(t, ctx, f, env.Path("b"), []byte("patched again\n"), fsys.WriteOptions{BackupSuffix: ".orig"})
	if d, _ := mustGet(t, env, "b.orig"); string(d) != "patched\n" {
		t.Errorf("second backup = %q", d)
	}
}

func backupSymlink(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "victim", []byte("precious\n"), 0o644)
	env.Put(t, "f", []byte("original\n"), 0o644)
	env.Symlink(t, "f.bak", "victim") // planted before the run
	mustWrite(t, ctx, f, env.Path("f"), []byte("patched\n"), fsys.WriteOptions{BackupSuffix: ".bak"})
	if d, _ := mustGet(t, env, "victim"); string(d) != "precious\n" {
		t.Errorf("a symlink planted as the backup path redirected the write: victim = %q", d)
	}
	if env.IsSymlink(t, "f.bak") {
		t.Error("the backup path is still a symlink; the backup must replace it")
	}
	if d, _ := mustGet(t, env, "f.bak"); string(d) != "original\n" {
		t.Errorf("f.bak = %q", d)
	}
	if d, _ := mustGet(t, env, "f"); string(d) != "patched\n" {
		t.Errorf("f = %q", d)
	}
}

func backupFailure(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "f", []byte("original\n"), 0o644)
	env.Mkdir(t, "f.bak") // a backup cannot be written over a directory
	before := env.Entries(t)
	err := f.Write(ctx, env.Path("f"), []byte("patched\n"), fsys.WriteOptions{BackupSuffix: ".bak"})
	if err == nil {
		t.Fatal("Write succeeded although its backup could not be written")
	}
	if d, _ := mustGet(t, env, "f"); string(d) != "original\n" {
		t.Errorf("the file was replaced although its backup failed: %q", d)
	}
	if extra := added(before, env.Entries(t)); len(extra) != 0 {
		t.Errorf("a failed Write left %v behind", extra)
	}
}

func invalidPaths(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	before := env.Entries(t)
	for _, p := range []string{"", "a\nb", "a\rb", "a\x00b", "tab\there", "esc\x1b[0m", "bell\a", "\xff\xfe", "c1\u0085control", "x\u007f"} {
		if _, err := f.Read(ctx, p); !errors.Is(err, fsys.ErrInvalidPath) {
			t.Errorf("Read(%q): %v, want fsys.ErrInvalidPath", p, err)
		}
		if err := f.Write(ctx, p, []byte("x"), fsys.WriteOptions{}); !errors.Is(err, fsys.ErrInvalidPath) {
			t.Errorf("Write(%q): %v, want fsys.ErrInvalidPath", p, err)
		}
	}
	env.Put(t, "ok", []byte("x"), 0o644)
	if err := f.Write(ctx, env.Path("ok"), []byte("y"), fsys.WriteOptions{BackupSuffix: ".b\nak"}); !errors.Is(err, fsys.ErrInvalidPath) {
		t.Errorf("a backup suffix with a newline: %v, want fsys.ErrInvalidPath", err)
	}
	if d, _ := mustGet(t, env, "ok"); string(d) != "x" {
		t.Errorf("a refused Write changed the file: %q", d)
	}
	if extra := added(append(before, "ok"), env.Entries(t)); len(extra) != 0 {
		t.Errorf("refused paths created %v", extra)
	}
}

func cancelled(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "x", []byte("old\n"), 0o644)
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Read(c, env.Path("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Read with a cancelled context: %v", err)
	}
	if err := f.Write(c, env.Path("x"), []byte("new\n"), fsys.WriteOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Write with a cancelled context: %v", err)
	}
	if d, _ := mustGet(t, env, "x"); string(d) != "old\n" {
		t.Errorf("a cancelled Write changed the file: %q", d)
	}
}

func externalChanges(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "e", []byte("one\n"), 0o644)
	if d := mustRead(t, ctx, f, env.Path("e")).Data; string(d) != "one\n" {
		t.Fatalf("Read = %q", d)
	}
	env.Put(t, "e", []byte("two\n"), 0o644)
	if d := mustRead(t, ctx, f, env.Path("e")).Data; string(d) != "two\n" {
		t.Errorf("Read after an outside change = %q: results must not be cached", d)
	}
}

func concurrent(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	const n = 8
	for i := 0; i < n; i++ {
		env.Put(t, fmt.Sprintf("con-%d", i), []byte(fmt.Sprintf("old %d\n", i)), 0o644)
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := env.Path(fmt.Sprintf("con-%d", i))
			for round := 0; round < 5; round++ {
				got, err := f.Read(ctx, p)
				if err != nil {
					errs <- err
					return
				}
				want := fmt.Sprintf("old %d\n", i)
				if round > 0 {
					want = fmt.Sprintf("new %d.%d\n", i, round-1)
				}
				if string(got.Data) != want {
					errs <- fmt.Errorf("goroutine %d round %d: read %q, want %q", i, round, got.Data, want)
					return
				}
				if err := f.Write(ctx, p, []byte(fmt.Sprintf("new %d.%d\n", i, round)), fsys.WriteOptions{}); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func closeTwice(t *testing.T, ctx context.Context, f fsys.FS, env Env) {
	env.Put(t, "x", []byte("a\n"), 0o644)
	mustRead(t, ctx, f, env.Path("x"))
	if err := f.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// DirEnv is an Env over a directory of the local machine.
type DirEnv struct{ Root string }

// NewDirEnv returns an Env over a fresh temporary directory.
func NewDirEnv(t testing.TB) *DirEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &DirEnv{Root: root}
}

func (e *DirEnv) Path(name string) string { return filepath.Join(e.Root, name) }

func (e *DirEnv) Rel(t testing.TB, name string) string {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(e.Root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	return name
}

func (e *DirEnv) Put(t testing.TB, name string, data []byte, mode fs.FileMode) {
	t.Helper()
	p := e.Path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func (e *DirEnv) Mkdir(t testing.TB, name string) {
	t.Helper()
	if err := os.MkdirAll(e.Path(name), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (e *DirEnv) Symlink(t testing.TB, link, target string) {
	t.Helper()
	l := e.Path(link)
	if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(l), e.Path(target))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, l); err != nil {
		t.Fatal(err)
	}
}

func (e *DirEnv) Get(t testing.TB, name string) ([]byte, fs.FileMode, bool) {
	t.Helper()
	p := e.Path(name)
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data, fi.Mode().Perm(), true
}

func (e *DirEnv) IsSymlink(t testing.TB, name string) bool {
	t.Helper()
	fi, err := os.Lstat(e.Path(name))
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func (e *DirEnv) Entries(t testing.TB) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(e.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != e.Root {
			rel, _ := filepath.Rel(e.Root, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}
