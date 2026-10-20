package fsys_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/fsys/fsystest"
)

func TestLocalContract(t *testing.T) {
	fsystest.Run(t, func(t *testing.T) (fsys.FS, fsystest.Env) {
		return fsys.Local(), fsystest.NewDirEnv(t)
	})
}

func TestLocalRefusesHugeFilesWithoutReadingThem(t *testing.T) {
	env := fsystest.NewDirEnv(t)
	p := env.Path("huge")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(fsys.MaxFileSize + 1); err != nil { // sparse: no disk is used
		t.Skipf("cannot make a sparse file here: %v", err)
	}
	f.Close()
	if _, err := fsys.Local().Read(context.Background(), p); !errors.Is(err, fsys.ErrTooLarge) {
		t.Errorf("err = %v, want fsys.ErrTooLarge", err)
	}
}
