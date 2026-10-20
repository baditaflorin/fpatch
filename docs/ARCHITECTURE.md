# Architecture

fpatch is small on purpose. This document explains how it is put together,
the rules the code keeps, how to add a file-system backend, and why a few
decisions went the way they did.

- [Overview](#overview)
- [Packages](#packages)
- [A run, step by step](#a-run-step-by-step)
- [The patch engine](#the-patch-engine)
- [The file-system layer](#the-file-system-layer)
- [Adding a backend](#adding-a-backend)
- [The ssh backend](#the-ssh-backend)
- [Decision record](#decision-record)
- [Testing strategy](#testing-strategy)

## Overview

```text
 stdin (spec) ──► spec.Parse ──► Spec{Hunks}
                                    │
 argv (files, flags) ──► cli ──► engine.Run ──► patch.Apply (pure: bytes in, bytes out)
                           │         │                 │
                           │         │                 └─► diff.Unified
                           │         ▼
                           │      fsys.FS  ◄── fsys.New("local" | "ssh:ALIAS" | ...)
                           ▼         │
                     text / JSON     ├─ local  (temp file + rename)
                      report         ├─ ssh    (one ssh session, remote sh helper)
                                     └─ memfs  (in-memory, for tests)
```

Everything that decides *what the new content is* is pure (no I/O) and lives
in `spec`, `patch` and `diff`. Everything that touches a disk lives behind
`fsys.FS`. `engine` is the only place that combines the two and it is where the
all-or-nothing rule is enforced.

## Packages

| Package | Role |
| --- | --- |
| `cmd/fpatch` | `main`: wires `os.Args`, stdio and the version string to `internal/cli`. Also holds the end-to-end tests of the built binary. |
| `internal/cli` | Flags, reading the spec, choosing the file system, text and JSON output, exit codes. |
| `internal/spec` | Parses the spec text into hunks; `Format` renders a spec back (used by the round-trip fuzz test). |
| `internal/patch` | `Apply(content, hunks)`: matching, counts, insertion points, sections, CRLF/BOM handling, idempotency, nearest-match search. Pure. |
| `internal/diff` | Unified diff (Myers) with "\ No newline at end of file" support. |
| `internal/engine` | Reads all files, applies the hunks, decides the exit code, writes in order, rolls back on failure. |
| `fsys` | The `FS` interface, the backend registry (`Register`, `New`), the `local` and `ssh` backends. **Public**: this is the extension point. |
| `fsys/memfs` | In-memory `FS` with failure injection, for tests. |
| `fsys/fsystest` | The contract test suite every backend must pass, and `DirEnv` for directory-backed stores. |

Only `fsys` and its subpackages are importable from outside the module;
everything else is `internal` so the command line and the spec format are the
only stable interfaces besides the backend contract.

## A run, step by step

`cli.Run` parses flags (which may come before, between or after the file
names), reads the spec, and parses it. Spec errors and usage errors are exit
`2` and happen before any file system is touched, so nothing connects to a
remote host for a typo. It then builds the `FS` with `fsys.New(target)` and
calls `engine.Run`.

`engine.Run`:

1. For each file named on the command line: `FS.Read`; refuse binary content
   (a NUL byte); `patch.Apply` with the hunks that apply to that file
   (`Spec.HunksFor`); render the diff. A file with no hunks is not even read.
   All files are processed even after a failure, so one run reports every
   problem.
2. If anything failed: write nothing. Exit `3` if there was an I/O error,
   else `1`.
3. If nothing changes: exit `4`. If `--dry-run`: report and stop.
4. Write the changed files one by one with `FS.Write`, passing the `Version`
   token from the `Read` as `ExpectVersion` so a concurrent change is refused
   (`ErrConflict`) rather than overwritten.
5. If a `Write` fails, restore the files already written from the in-memory
   originals (with its own 30 s budget, even if the run's context expired),
   and report each file's fate. A restore that fails is reported loudly.

The engine never opens a file itself and never builds a path; the strings in
the report are the ones that came from the command line.

## The patch engine

`patch.Apply` has a few rules worth knowing before changing it:

- **Original coordinates.** Every hunk is matched against the file as it was
  before the run, and produces *edits* (`start`, `end`, `text`) in those
  coordinates. After all hunks succeed, the edits are sorted and checked for
  overlap, then spliced in one pass. This makes line numbers in messages
  refer to the file on disk and hunk order irrelevant. The price: a hunk
  cannot anchor on text another hunk creates (it reports `missing`).
- **Fail whole.** Any failed hunk makes `Apply` return no content at all.
- **Normalise, then restore.** CRLF-only files are converted to LF before
  matching and converted back after; a BOM is removed and re-added; files with
  mixed endings are matched as they are. `Result.Before` and `Result.After`
  are the normalised texts, which is what the diff shows.
- **Matching is `strings.Index`.** Counts use non-overlapping occurrences, the
  same as `strings.Count`; the fuzz test checks `Apply` against
  `strings.Replace` for the plain case.
- **insert-after / insert-before are line oriented**: `insertionPoint` maps the
  anchor to a line boundary. A missing final newline is preserved.
- **upsert-section** scans ATX headings outside fenced code blocks, finds the
  section's range, and keeps the existing heading line and trailing blank
  lines.
- **Idempotency** is decided per hunk type from the original text (see the
  README for the rules); it is a heuristic and says so.
- **Nearest match** scores every window of the file against the anchor with the
  Dice coefficient of byte bigrams per line (whitespace collapsed), takes the
  mean over the anchor's non-blank lines of the best line in the window, and
  reports the best window if it scores at least 0.4. A cost cap
  (`maxCells`) skips the search on enormous inputs instead of stalling.

## The file-system layer

```go
type FS interface {
    Read(ctx context.Context, path string) (File, error)
    Write(ctx context.Context, path string, data []byte, opts WriteOptions) error
    Close() error
}
```

`File` carries `Data`, an opaque `Version` token and the permission `Mode`
(0 when unknown). `WriteOptions` carries `ExpectVersion` and `BackupSuffix`.

The contract, which `fsystest.Run` checks for every backend:

- `Read` returns the exact bytes of a regular file and follows symlinks.
  Missing files give `fs.ErrNotExist`, other file types `ErrNotRegular`, huge
  files `ErrTooLarge`.
- `Write` replaces an existing regular file atomically, keeps its permission
  bits, follows symlinks (the link stays a link), **never creates** a missing
  file, and on failure leaves the file as it was and no temporary files.
- A non-empty `ExpectVersion` that no longer matches gives `ErrConflict` and
  changes nothing (backends that return an empty `Version` cannot offer this
  and ignore it).
- A `BackupSuffix` saves the previous content at `path+suffix` with the same
  mode, *before* the replacement; if that fails the file is left alone. The
  backup sits next to the name that was given, not next to a symlink's target.
- Paths with control characters, empty paths and invalid UTF-8 are refused
  with `ErrInvalidPath` before anything runs (`fsys.ValidatePath`).
- `ctx` cancellation is honoured. `Close` may be called twice.

### The registry

`fsys` keeps a registry the way `database/sql` does:

```go
fsys.Register("name", func(arg string) (fsys.FS, error) { ... })  // in init()
fs, err := fsys.New("name:arg")                                   // "ssh:web1" -> name "ssh", arg "web1"
```

`New("")` and `New("local")` give the local file system. A target is split at
the first colon, so an argument may itself contain colons. `Register` panics
on an empty or duplicate name, like `database/sql`, because those are
programming errors found at start-up. `Names()` lists what is registered; the
"unknown file system" error uses it. On the command line, `--fs name[:arg]`
reaches any backend and `--host ALIAS` is shorthand for `--fs ssh:ALIAS`.

## Adding a backend

1. Create a file in `fsys/` (or your own package) with a type implementing
   `fsys.FS`, and register it:

   ```go
   package fsys

   func init() {
       Register("myfs", func(arg string) (FS, error) { return newMyFS(arg) })
   }
   ```

   Do the cheap argument validation in the factory, and connect lazily on the
   first `Read` or `Write`, so a typo in a spec does not open a connection.

2. Map errors onto the sentinels with `%w`: `fs.ErrNotExist`,
   `fs.ErrPermission`, `ErrNotRegular`, `ErrTooLarge`, `ErrConflict`,
   `ErrInvalidPath`. Call `ValidatePath` on every path and backup path.

3. Return a `Version` from `Read` if you can detect a concurrent change (a
   content hash is the simplest), and honour `ExpectVersion` in `Write`.

4. Run the contract suite in a test in package `fsys_test`:

   ```go
   func TestMyFSContract(t *testing.T) {
       fsystest.Run(t, func(t *testing.T) (fsys.FS, fsystest.Env) {
           env := fsystest.NewDirEnv(t)   // or your own Env over your store
           return newMyFS(env.Root), env
       })
   }
   ```

   `Env` is how the suite looks at the store behind your back (put a file,
   read one, make a symlink, list entries). `DirEnv` does that for any backend
   whose store is a local directory, however the backend reaches it. The ssh
   backend's test is the worked example: a fake `ssh` that runs the remote
   command locally.

5. Document the target syntax in the README's flags table.

That is all the engine needs; nothing else in fpatch knows which backend it is
talking to.

## The ssh backend

The backend starts **one** `ssh` process per run:

```text
ssh -T -o BatchMode=yes -o ClearAllForwardings=yes -o RemoteCommand=none \
    -o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 \
    -- ALIAS sh
```

The alias is validated against a narrow pattern and placed after `--`, so it
can never be read as an option. The remote command is a bare `sh`, which any
login shell can run.

Over that session fpatch sends, on stdin of the remote `sh`:

1. `FPT=<random 128-bit token>` and the helper script (`fsys/ssh_prelude.sh`,
   embedded in the binary), which defines `fp_read` and `fp_write`.
2. One request per operation.

Every reply starts on a fresh line with the token (`<token> <kind> ...`), so
anything the remote login scripts print can be skipped, and file content cannot
be mistaken for a reply (the token is unguessable and never appears in
content).

**Read**: `fp_read 'path'`. The reply is `<token> ok <mode>`, then the file
bytes, then `\n<token> end <status>`; the final `\n` is part of the framing and
is stripped. Other replies: `notfound`, `notregular`, `denied`,
`err <code> <message>`.

**Write**: `fp_write 'path' SIZE 'expect' 'suffix' <<'FPATCH_EOF_<random>'`
followed by the content and the delimiter line. The delimiter is random and
checked not to occur in the data, so the here-document is verbatim (a quoted
delimiter disables every expansion). A here-document always ends in a newline,
so content that does not end in one is sent with an extra `\n` and trimmed on
arrival with `head -c SIZE`; the staged size is checked against `SIZE`. The
helper then: resolves symlinks, refuses non-regular files, compares
`cksum` with `expect` (conflict detection), stages into a `mktemp` file in the
target's directory, sets owner (best effort) and mode (from `stat`), makes the
backup with `cp -p` if asked, and `mv -f` the new file over the target. Reply
`done` or `err <code> <message>`.

Paths and options are passed as single-quoted arguments (`shQuote`) of a
function in a shell that is not the login shell, after `ValidatePath` has
refused NUL and control characters. The quoting is tested against a real shell
with a property test and a fuzz target. The content of a file never reaches the
shell as code, only as a quoted here-document.

The version token for conflict detection is the POSIX `cksum` pair (`CRC
SIZE`), computed locally by `posixCksum` (a few lines of Go, cross-checked
against the real `cksum` command in tests) and recomputed remotely just
before the new content is staged.

If the connection fails, the error carries ssh's own stderr (permission denied,
could not resolve host) and the failure is sticky: later operations fail the
same way without retrying. Context cancellation kills the ssh process.

## Decision record

### Standard library only; go-common considered and not used

fpatch is tiny and public. The maintainer's public library
`github.com/baditaflorin/go-common` was reviewed (its `main` branch, October
2026) for anything cheap to reuse: atomic file writes, diffs, a version flag.

- It has no atomic-write helper or diff package. The `rename` calls in it
  belong to unrelated components.
- Its `version` package would be the only candidate, and fpatch's version
  handling is ten lines (an `-X` linker flag, falling back to the module
  version recorded by `go install`).
- `go-common` is one large module whose `go` directive and dependency graph
  (databases, tracing, service frameworks) would become every fpatch consumer's
  problem, and its own minimum Go version would raise fpatch's floor, which is
  currently Go 1.22 so that `go install` works on older toolchains.

So there is no dependency, `go.mod` has no `require` lines, and the binary is
built with `CGO_ENABLED=0`. Revisit this only if go-common offers something
fpatch needs that is clearly worth that coupling, and put it behind a build tag
or a separate module rather than in the core.

### Hunks match the original file

Sequential application (each hunk sees the previous result) is what ad-hoc
`s = s.replace(...)` scripts do, but it makes line numbers in a miss report
meaningless (they refer to an intermediate buffer) and makes results depend on
hunk order. Matching against the original and splicing edits once gives stable
messages and independence, and turns accidental overlap into an explicit error.

### One connection per run

A new ssh process per file would multiply handshakes, and on a busy laptop
also local ports. One session with a request/reply protocol costs a small
helper script, but keeps "one connection" true even without ControlMaster.

### No file creation, no regex, no deletion

Each would widen what a typo can do. fpatch edits existing regular files by
exact text; the shell is better at the rest.

### Paths come only from argv

A spec is data produced by a program or a model; the command line is what a
reviewer or a policy hook sees. `=== file PATH` can therefore only *select*
among the arguments, never add a path.

### Public `fsys`, internal everything else

The backend contract is the extension point, so it is public and documented.
The rest can change freely between versions.

## Testing strategy

- **Unit and table tests** for every hunk type and every exit code, including
  counts, ambiguity, miss and nearest-match output, idempotency, CRLF, BOM,
  mixed endings and missing final newlines.
- **Atomicity tests** on an in-memory file system with failure injection: a
  failing second hunk, a failing second file, a failing write, a failing
  rollback and a concurrent modification each leave exactly the state the
  documentation promises.
- **Contract tests** (`fsystest`) run against `local`, `ssh` and `memfs`: the
  same cases for each, including awkward file names and shell metacharacters,
  and names that would create a file if a shell ever interpreted them.
- **The ssh backend runs the real protocol**: tests put a stand-in `ssh` first
  in `PATH` that runs the remote command through `/bin/sh -c` locally, so the
  embedded helper script and real coreutils are exercised without a network.
  Failure modes (unreachable host, permission denied, noisy login, hanging
  connection) are simulated by the stand-in.
- **Fuzzing**: the spec parser (round trip through `Format`), the replace
  matcher against `strings.Replace`, the diff (apply the produced diff with an
  independent reader and compare), and the shell quoting against a real shell.
- **End-to-end tests** build the binary and drive it with real stdin and files,
  including `--host` through the stand-in `ssh`.

Some tests (permission denied, unreadable file) skip when run as root, because
root ignores permissions; run the suite as a normal user as well.
