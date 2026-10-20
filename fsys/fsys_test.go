package fsys_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/fpatch/fsys"
	"github.com/baditaflorin/fpatch/fsys/memfs"
)

var registerOnce sync.Once

func registerTestBackend(t *testing.T) {
	t.Helper()
	registerOnce.Do(func() {
		fsys.Register("registry-test", func(arg string) (fsys.FS, error) {
			if arg == "boom" {
				return nil, errors.New("the factory failed")
			}
			m := memfs.New()
			m.Put("arg", []byte(arg), 0o644)
			return m, nil
		})
	})
}

func TestRegistryBuildsBackendsByName(t *testing.T) {
	registerTestBackend(t)
	f, err := fsys.New("registry-test:some:arg:with:colons")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Read(context.Background(), "arg")
	if err != nil || string(got.Data) != "some:arg:with:colons" {
		t.Errorf("the factory must get everything after the first colon: %q, %v", got.Data, err)
	}
	f, err = fsys.New("registry-test")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Read(context.Background(), "arg"); len(got.Data) != 0 {
		t.Errorf("a target without a colon must give an empty arg, got %q", got.Data)
	}
	if _, err := fsys.New("registry-test:boom"); err == nil || !strings.Contains(err.Error(), "the factory failed") {
		t.Errorf("a factory error must come back as is: %v", err)
	}
}

func TestRegistryDefaultsToLocal(t *testing.T) {
	for _, target := range []string{"", "local"} {
		f, err := fsys.New(target)
		if err != nil {
			t.Fatalf("New(%q): %v", target, err)
		}
		f.Close()
	}
	if _, err := fsys.New("local:/tmp"); err == nil {
		t.Error("the local file system must not accept an argument")
	}
}

func TestRegistryUnknownBackendListsTheKnownOnes(t *testing.T) {
	registerTestBackend(t)
	_, err := fsys.New("nosuch:x")
	if err == nil {
		t.Fatal("an unknown backend was accepted")
	}
	for _, want := range []string{"nosuch", "local", "ssh", "registry-test"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	names := fsys.Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("Names is not sorted: %v", names)
		}
	}
}

func TestRegisterPanicsOnMistakes(t *testing.T) {
	registerTestBackend(t)
	ok := func(string) (fsys.FS, error) { return nil, nil }
	cases := map[string]func(){
		"duplicate":   func() { fsys.Register("local", ok) },
		"empty name":  func() { fsys.Register("", ok) },
		"colon":       func() { fsys.Register("a:b", ok) },
		"nil factory": func() { fsys.Register("never-registered", nil) },
	}
	for name, fn := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Register did not panic", name)
				}
			}()
			fn()
		}()
	}
}

func TestValidatePath(t *testing.T) {
	for _, p := range []string{"a", "/etc/app/config.yml", "dir with space/f", "ünï/日本", "-x", "a'b", "$HOME"} {
		if err := fsys.ValidatePath(p); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	for _, p := range []string{"", "a\nb", "a\x00b", "a\tb", "a\x1bb", "a\u0085b", "a\x7fb", "\xff"} {
		if err := fsys.ValidatePath(p); !errors.Is(err, fsys.ErrInvalidPath) {
			t.Errorf("%q: err = %v, want ErrInvalidPath", p, err)
		}
	}
}
