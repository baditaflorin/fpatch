//go:build unix

package fsys_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/fsys/fsystest"
)

func TestLocalKeepsSpecialModeBits(t *testing.T) {
	env := fsystest.NewDirEnv(t)
	env.Put(t, "prog", []byte("old\n"), 0o755|fs.ModeSetuid)
	before, _ := os.Stat(env.Path("prog"))
	if before.Mode()&fs.ModeSetuid == 0 {
		t.Skip("this file system does not keep the setuid bit")
	}
	if err := fsys.Local().Write(context.Background(), env.Path("prog"), []byte("new\n"), fsys.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(env.Path("prog"))
	if after.Mode() != before.Mode() {
		t.Errorf("mode %v became %v", before.Mode(), after.Mode())
	}
}

func TestLocalRefusesNamedPipes(t *testing.T) {
	env := fsystest.NewDirEnv(t)
	p := env.Path("fifo")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skipf("no mkfifo: %v", err)
	}
	// A bad implementation would block here forever.
	if _, err := fsys.Local().Read(context.Background(), p); !errors.Is(err, fsys.ErrNotRegular) {
		t.Errorf("Read of a fifo: %v, want fsys.ErrNotRegular", err)
	}
	if err := fsys.Local().Write(context.Background(), p, []byte("x"), fsys.WriteOptions{}); !errors.Is(err, fsys.ErrNotRegular) {
		t.Errorf("Write to a fifo: %v, want fsys.ErrNotRegular", err)
	}
}

func TestLocalFailedWriteLeavesTheFileAndNoTemporaryFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	env := fsystest.NewDirEnv(t)
	env.Put(t, "ro/file", []byte("old\n"), 0o644)
	if err := os.Chmod(env.Path("ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(env.Path("ro"), 0o755) })
	err := fsys.Local().Write(context.Background(), env.Path("ro/file"), []byte("new\n"), fsys.WriteOptions{})
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want fs.ErrPermission", err)
	}
	if d, _, _ := env.Get(t, "ro/file"); string(d) != "old\n" {
		t.Errorf("the file changed: %q", d)
	}
	if entries := env.Entries(t); len(entries) != 2 { // ro, ro/file
		t.Errorf("entries = %v", entries)
	}
}

func TestLocalUnreadableFileIsAPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	env := fsystest.NewDirEnv(t)
	env.Put(t, "secret", []byte("x"), 0o000)
	if _, err := fsys.Local().Read(context.Background(), env.Path("secret")); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want fs.ErrPermission", err)
	}
}
