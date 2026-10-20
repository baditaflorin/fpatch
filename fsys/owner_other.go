//go:build !unix

package fsys

import "os"

// preserveOwner is a no-op where there is no Unix ownership model.
func preserveOwner(f *os.File, like os.FileInfo) {}
