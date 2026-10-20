// Package fsys is the file-access layer of fpatch.
//
// fpatch never touches the file system directly. Everything it reads or
// writes goes through the small FS interface below, and implementations are
// looked up by name in a registry (the same pattern as database/sql drivers):
//
//	fsys.Register("name", factory)   // done by a backend, usually in init()
//	fs, err := fsys.New("name:arg")  // done by the caller
//
// Two backends ship with the program: "local" (the default) and "ssh:ALIAS".
// The package github.com/baditaflorin/fpatch/fsys/fsystest holds a contract
// test suite that every backend must pass, and fsys/memfs is an in-memory
// backend for tests.
package fsys

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// MaxFileSize is the largest file a backend will read or write. Larger files
// fail with ErrTooLarge instead of being pulled into memory.
const MaxFileSize = 64 << 20

// Errors a backend returns (wrapped, so test with errors.Is). A backend maps
// its native "no such file" and "permission denied" conditions onto
// fs.ErrNotExist and fs.ErrPermission.
var (
	// ErrNotRegular is returned for directories, devices, sockets and the like.
	ErrNotRegular = errors.New("not a regular file")
	// ErrConflict is returned by Write when the file no longer has the version
	// the caller read (see WriteOptions.ExpectVersion).
	ErrConflict = errors.New("file changed since it was read")
	// ErrTooLarge is returned for files above MaxFileSize.
	ErrTooLarge = errors.New("file too large")
	// ErrConnection is returned when a backend lost its connection to the
	// store (or never got one). A Write that fails with it may or may not have
	// taken effect, which is why the engine checks the file before it decides
	// how to undo a run.
	ErrConnection = errors.New("connection to the remote side failed")
	// ErrInvalidPath is returned for empty paths and paths with control
	// characters or invalid UTF-8.
	ErrInvalidPath = errors.New("invalid path")
)

// File is what Read returns.
type File struct {
	// Data is the exact content of the file.
	Data []byte
	// Version is an opaque token describing this content. Passing it back in
	// WriteOptions.ExpectVersion makes Write fail with ErrConflict if the file
	// changed in the meantime. Empty when the backend cannot offer the check.
	Version string
	// Mode holds the permission bits, or 0 when the backend cannot tell.
	Mode fs.FileMode
}

// WriteOptions tune a Write.
type WriteOptions struct {
	// ExpectVersion, when not empty, makes the write conditional: if the file
	// does not have this version any more, Write returns ErrConflict and
	// changes nothing. Backends that return an empty File.Version ignore it.
	ExpectVersion string
	// BackupSuffix, when not empty, saves the previous content at
	// path+BackupSuffix (same mode) before the file is replaced. If the backup
	// cannot be written, the file is left untouched.
	BackupSuffix string
}

// FS is the file system an fpatch run works on. One FS value serves one run;
// implementations may keep a connection open until Close.
//
// The contract every implementation must keep (fsystest checks it):
//
//   - Read returns the exact bytes of a regular file, following symlinks.
//     Missing files give fs.ErrNotExist, directories and other special files
//     give ErrNotRegular, files above MaxFileSize give ErrTooLarge.
//   - Write replaces the content of an existing regular file atomically
//     (readers see the old or the new content, never a mix), keeps its
//     permission bits, and follows symlinks: the link stays a link and its
//     target gets the new content. Write never creates a missing file; that
//     gives fs.ErrNotExist. A failed Write leaves the file as it was and
//     leaves no temporary files behind.
//   - Paths with control characters, and empty paths, are refused with
//     ErrInvalidPath before anything runs.
//   - Methods honour ctx cancellation and are safe to call one at a time from
//     any goroutine. Close may be called more than once.
type FS interface {
	Read(ctx context.Context, path string) (File, error)
	Write(ctx context.Context, path string, data []byte, opts WriteOptions) error
	Close() error
}

// Factory builds an FS. arg is everything after the first ":" of the target
// ("web1" for "ssh:web1"), or "" when the target had no colon.
type Factory func(arg string) (FS, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a backend available under name. It panics if the name is
// empty, contains ":", is already taken, or f is nil, like database/sql's
// Register, because all of those are programming errors found at start-up.
func Register(name string, f Factory) {
	if name == "" || strings.Contains(name, ":") {
		panic(fmt.Sprintf("fsys: invalid backend name %q", name))
	}
	if f == nil {
		panic("fsys: Register factory is nil for " + name)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("fsys: Register called twice for backend " + name)
	}
	factories[name] = f
}

// Names lists the registered backends in sorted order.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// New builds an FS from a target string of the form "name" or "name:arg".
// The empty target means "local".
func New(target string) (FS, error) {
	name, arg := target, ""
	if i := strings.IndexByte(target, ':'); i >= 0 {
		name, arg = target[:i], target[i+1:]
	}
	if name == "" {
		name = "local"
	}
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown file system %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return f(arg)
}

// ValidatePath is the check every backend applies to a path before use. It
// rejects empty paths, control characters (which includes newlines and NUL)
// and invalid UTF-8, so a path can never smuggle a second command or line
// into a protocol.
func ValidatePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	if !utf8.ValidString(p) {
		return fmt.Errorf("%w: %q is not valid UTF-8", ErrInvalidPath, p)
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %q contains a control character", ErrInvalidPath, p)
		}
	}
	return nil
}
