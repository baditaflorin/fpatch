package fsys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func init() {
	Register("local", func(arg string) (FS, error) {
		if arg != "" {
			return nil, fmt.Errorf("the local file system takes no argument, got %q", arg)
		}
		return Local(), nil
	})
}

// Local returns the FS for the machine fpatch runs on.
func Local() FS { return localFS{} }

type localFS struct{}

func (localFS) Close() error { return nil }

// permBits are the mode bits Write preserves.
const permBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

func contentVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (localFS) Read(ctx context.Context, path string) (File, error) {
	if err := ValidatePath(path); err != nil {
		return File{}, err
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return File{}, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return File{}, err
	}
	if !fi.Mode().IsRegular() {
		return File{}, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	if fi.Size() > MaxFileSize {
		return File{}, fmt.Errorf("%s: %w (%d bytes, limit %d)", path, ErrTooLarge, fi.Size(), MaxFileSize)
	}
	f, err := os.Open(real)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return File{}, err
	}
	if len(data) > MaxFileSize {
		return File{}, fmt.Errorf("%s: %w", path, ErrTooLarge)
	}
	return File{Data: data, Version: contentVersion(data), Mode: fi.Mode() & permBits}, nil
}

func (localFS) Write(ctx context.Context, path string, data []byte, opts WriteOptions) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	if opts.BackupSuffix != "" {
		if err := ValidatePath(path + opts.BackupSuffix); err != nil {
			return err
		}
	}
	if len(data) > MaxFileSize {
		return fmt.Errorf("%s: %w", path, ErrTooLarge)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err // missing files, including dangling links, give fs.ErrNotExist
	}
	fi, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	if opts.ExpectVersion != "" || opts.BackupSuffix != "" {
		current, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		if opts.ExpectVersion != "" && contentVersion(current) != opts.ExpectVersion {
			return fmt.Errorf("%s: %w", path, ErrConflict)
		}
		if opts.BackupSuffix != "" {
			if err := writeBackup(path+opts.BackupSuffix, current, fi); err != nil {
				return fmt.Errorf("backup of %s: %w", path, err)
			}
		}
	}
	return atomicReplace(real, data, fi)
}

// writeBackup stores data at path. Whatever is already there is replaced, a
// symlink included (the rename replaces the link, it does not follow it), so a
// FILE.bak that someone planted as a link cannot redirect the write elsewhere.
func writeBackup(path string, data []byte, like os.FileInfo) error {
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	return atomicReplace(filepath.Join(dir, filepath.Base(path)), data, like)
}

// atomicReplace writes data to a temporary file in the directory of target and
// renames it over target, keeping the permission bits (and, when permitted,
// the owner) of like. The temporary file never outlives the call.
func atomicReplace(target string, data []byte, like os.FileInfo) (err error) {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".fpatch-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	preserveOwner(tmp, like) // before chmod: chown can clear setuid bits
	if err = tmp.Chmod(like.Mode() & permBits); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, target); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes the rename durable where the platform allows it; failures are
// ignored because the rename itself already succeeded.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}
