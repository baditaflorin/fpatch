// Package memfs is an in-memory fsys.FS for tests. Besides the FS methods it
// has helpers to set up files, symlinks and directories, to inspect the result,
// and to inject failures, so tests of code built on fsys.FS can exercise
// error paths without touching a disk.
package memfs

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"sync"

	"github.com/baditaflorin/fpatch/fsys"
)

type node struct {
	data    []byte
	mode    fs.FileMode
	version int
}

// FS is an in-memory file system. The zero value is not usable; call New.
type FS struct {
	mu      sync.Mutex
	files   map[string]*node
	links   map[string]string
	dirs    map[string]bool
	failing map[string]error
	writes  []string
	closed  int
	// AfterRead, when set, runs after every successful Read with the cleaned
	// path, while no lock is held. Tests use it to change a file behind the
	// caller's back and provoke fsys.ErrConflict.
	AfterRead func(path string)
}

// New returns an empty file system.
func New() *FS {
	return &FS{
		files:   map[string]*node{},
		links:   map[string]string{},
		dirs:    map[string]bool{},
		failing: map[string]error{},
	}
}

func clean(p string) string { return path.Clean(p) }

// Put creates or replaces a file, bypassing all checks.
func (m *FS) Put(p string, data []byte, mode fs.FileMode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p = clean(p)
	n := m.files[p]
	if n == nil {
		n = &node{}
		m.files[p] = n
	}
	n.data = append([]byte(nil), data...)
	n.mode = mode
	n.version++
}

// Mkdir marks p as a directory.
func (m *FS) Mkdir(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[clean(p)] = true
}

// Symlink makes link point at target (both are plain keys in the map).
func (m *FS) Symlink(link, target string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.links[clean(link)] = clean(target)
}

// IsSymlink reports whether p is a symlink.
func (m *FS) IsSymlink(p string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.links[clean(p)]
	return ok
}

// Get returns the content and mode of the file at p, without following links.
func (m *FS) Get(p string) (data []byte, mode fs.FileMode, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.files[clean(p)]
	if n == nil {
		return nil, 0, false
	}
	return append([]byte(nil), n.data...), n.mode, true
}

// Paths lists every file, symlink and directory, sorted.
func (m *FS) Paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for p := range m.files {
		out = append(out, p)
	}
	for p := range m.links {
		out = append(out, p)
	}
	for p := range m.dirs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// FailWrites makes every later Write to p (after symlink resolution) return
// err without changing anything. A nil err removes the failure.
func (m *FS) FailWrites(p string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.failing, clean(p))
		return
	}
	m.failing[clean(p)] = err
}

// Writes lists the paths of successful Write calls, in order, including
// backups. Tests use it to assert that nothing was written.
func (m *FS) Writes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.writes...)
}

// CloseCount reports how many times Close was called.
func (m *FS) CloseCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// resolve follows symlinks; callers hold the lock.
func (m *FS) resolve(p string) (string, error) {
	p = clean(p)
	for i := 0; i < 40; i++ {
		t, ok := m.links[p]
		if !ok {
			return p, nil
		}
		p = t
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", p)
}

func version(n *node) string { return strconv.Itoa(n.version) }

func (m *FS) Read(ctx context.Context, p string) (fsys.File, error) {
	if err := fsys.ValidatePath(p); err != nil {
		return fsys.File{}, err
	}
	if err := ctx.Err(); err != nil {
		return fsys.File{}, err
	}
	m.mu.Lock()
	real, err := m.resolve(p)
	if err != nil {
		m.mu.Unlock()
		return fsys.File{}, err
	}
	n := m.files[real]
	switch {
	case n != nil:
	case m.dirs[real]:
		m.mu.Unlock()
		return fsys.File{}, fmt.Errorf("%s: %w", p, fsys.ErrNotRegular)
	default:
		m.mu.Unlock()
		return fsys.File{}, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	}
	if len(n.data) > fsys.MaxFileSize {
		m.mu.Unlock()
		return fsys.File{}, fmt.Errorf("%s: %w", p, fsys.ErrTooLarge)
	}
	f := fsys.File{Data: append([]byte(nil), n.data...), Version: version(n), Mode: n.mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)}
	hook := m.AfterRead
	m.mu.Unlock()
	if hook != nil {
		hook(clean(p))
	}
	return f, nil
}

func (m *FS) Write(ctx context.Context, p string, data []byte, opts fsys.WriteOptions) error {
	if err := fsys.ValidatePath(p); err != nil {
		return err
	}
	if opts.BackupSuffix != "" {
		if err := fsys.ValidatePath(p + opts.BackupSuffix); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	real, err := m.resolve(p)
	if err != nil {
		return err
	}
	n := m.files[real]
	switch {
	case n != nil:
	case m.dirs[real]:
		return fmt.Errorf("%s: %w", p, fsys.ErrNotRegular)
	default:
		return fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	}
	if opts.ExpectVersion != "" && opts.ExpectVersion != version(n) {
		return fmt.Errorf("%s: %w", p, fsys.ErrConflict)
	}
	if err := m.failing[real]; err != nil {
		return err
	}
	if opts.BackupSuffix != "" {
		bak := clean(p + opts.BackupSuffix)
		if m.dirs[bak] {
			return fmt.Errorf("backup %s: %w", bak, fsys.ErrNotRegular)
		}
		if err := m.failing[bak]; err != nil {
			return err
		}
		delete(m.links, bak) // a backup replaces a symlink, it does not follow it
		b := m.files[bak]
		if b == nil {
			b = &node{}
			m.files[bak] = b
		}
		b.data = append([]byte(nil), n.data...)
		b.mode = n.mode
		b.version++
		m.writes = append(m.writes, bak)
	}
	n.data = append([]byte(nil), data...)
	n.version++
	m.writes = append(m.writes, real)
	return nil
}

func (m *FS) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed++
	return nil
}
