package memfs_test

import (
	"io/fs"
	"testing"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/fsys/fsystest"
	"github.com/baditaflorin/fpatch/fsys/memfs"
)

// env lets the contract suite see the memfs store.
type env struct{ m *memfs.FS }

func (e env) Path(name string) string                   { return name }
func (e env) Rel(t testing.TB, name string) string      { return name }
func (e env) Mkdir(t testing.TB, name string)           { e.m.Mkdir(name) }
func (e env) Symlink(t testing.TB, link, target string) { e.m.Symlink(link, target) }
func (e env) IsSymlink(t testing.TB, name string) bool  { return e.m.IsSymlink(name) }
func (e env) Entries(t testing.TB) []string             { return e.m.Paths() }
func (e env) Put(t testing.TB, name string, data []byte, mode fs.FileMode) {
	e.m.Put(name, data, mode)
}
func (e env) Get(t testing.TB, name string) ([]byte, fs.FileMode, bool) { return e.m.Get(name) }

func TestContract(t *testing.T) {
	fsystest.Run(t, func(t *testing.T) (fsys.FS, fsystest.Env) {
		m := memfs.New()
		return m, env{m}
	})
}

func TestHelpers(t *testing.T) {
	m := memfs.New()
	m.Put("a", []byte("x"), 0o644)
	m.Symlink("l", "a")
	m.Mkdir("d")
	if got := m.Paths(); len(got) != 3 || got[0] != "a" || got[1] != "d" || got[2] != "l" {
		t.Errorf("Paths = %v", got)
	}
	if _, _, ok := m.Get("l"); ok {
		t.Error("Get must not follow symlinks")
	}
	m.Close()
	m.Close()
	if m.CloseCount() != 2 {
		t.Errorf("CloseCount = %d", m.CloseCount())
	}
}
