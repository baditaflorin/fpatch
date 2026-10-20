# fpatch

An exact-match, atomic, all-or-nothing file patcher for agents and scripts.

You describe the edit in a small text spec on stdin (a heredoc works, no
quoting gymnastics). Every hunk has to match the file exactly as many times as
you said it should. If any hunk in any file does not, **nothing is written**,
and fpatch shows you the nearest text it did find, with line numbers, so you
can fix the anchor.

```sh
fpatch /etc/app/config.yml <<'EOF'
=== replace
timeout: 30
=== with
timeout: 60
=== end
EOF
```

```text
--- /etc/app/config.yml
+++ /etc/app/config.yml
@@ -1,5 +1,5 @@
 # app config
 server:
   host: example.org
-  timeout: 30
+  timeout: 60
   retries: 3
fpatch: applied 1 hunk in 1 file
```

It is one static Go binary with no dependencies beyond the standard library.
Files are read and written through a small interface, so the same patch can
run on the local disk or on a remote host over `ssh` (no remote install), and
other backends can be added (see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)).

- [Install](#install)
- [Quick start](#quick-start)
- [Spec reference](#spec-reference)
- [Flags](#flags)
- [Exit codes](#exit-codes)
- [What a miss looks like](#what-a-miss-looks-like)
- [JSON output](#json-output)
- [Remote hosts](#remote-hosts)
- [What "atomic" means here](#what-atomic-means-here)
- [When to use fpatch vs an editor tool or sed](#when-to-use-fpatch-vs-an-editor-tool-or-sed)
- [Limitations](#limitations)
- [Development](#development)

## Install

With a Go toolchain (Go 1.22 or newer), once a version is tagged:

```sh
go install github.com/baditaflorin/fpatch/cmd/fpatch@v0.1.0
```

From a checkout:

```sh
make build                # ./fpatch for this machine
make dist VERSION=v0.1.0  # darwin/linux x amd64/arm64 archives in dist/
```

`make dist` writes one archive per target, `dist/fpatch_0.1.0_darwin_arm64.tar.gz`
and so on (the binary, `LICENSE` and `README.md` at the top level of each), and
`dist/checksums.txt` in `<sha256>  <file>` format. It needs only Go, `tar`,
`gzip` and `sha256sum` or `shasum`. It builds locally and publishes nothing;
releases are described in [CONTRIBUTING.md](CONTRIBUTING.md).

`fpatch --version` prints the bare version number (`0.1.0`, the number in the
archive names) and nothing else. A binary installed with `go install` reports
its module version without the leading `v`; a plain `make build` reports
`git describe`.

## Quick start

The spec comes from stdin, and the files to patch are the arguments:

```sh
fpatch FILE... [flags] < spec
```

Look first, then write:

```sh
fpatch --dry-run app.conf <<'EOF'
=== replace
log_level = info
=== with
log_level = debug
=== end
EOF
```

Patch several files together. They are checked as a group, so either all of
them change or none does:

```sh
fpatch a.conf b.conf c.conf <<'EOF'
=== replace
version = 1.4
=== with
version = 1.5
=== end
EOF
```

Different edits for different files in one all-or-nothing run:

```sh
fpatch api.env web.env <<'EOF'
=== file api.env
=== replace
PORT=8080
=== with
PORT=9090
=== end
=== file web.env
=== append
API_URL=http://localhost:9090
=== end
EOF
```

Run it on another machine (an alias from your `~/.ssh/config`, so ProxyJump
and ControlMaster keep working):

```sh
fpatch --host web1 /etc/app/config.yml <<'EOF'
=== upsert-section "## Limits"

max_connections = 200
=== end
EOF
```

Safe to run twice (a hunk that is already applied counts as success):

```sh
fpatch --idempotent /etc/app/config.yml < spec.txt   # exit 0 first time, 4 after
```

## Spec reference

A spec is a list of blocks. A directive line starts at column 0 with `===`, a
space and a keyword. Blank lines and `#` comment lines between blocks are
ignored. Anything else outside a block is an error, so a heredoc typo cannot
silently do nothing.

| Block | Does |
| --- | --- |
| `=== replace [count=N\|all]` ... `=== with` ... `=== end` | Replace the old text with the new text. |
| `=== insert-after [count=N\|all]` ... `=== with` ... `=== end` | Insert whole lines after the line where the anchor ends. |
| `=== insert-before [count=N\|all]` ... `=== with` ... `=== end` | Insert whole lines before the line where the anchor starts. |
| `=== append` ... `=== end` | Add lines at the end of the file. |
| `=== upsert-section "## Heading"` ... `=== end` | Replace the body of a Markdown section, or add the section at the end. |
| `=== file PATH` | The hunks below apply to `PATH` only. |

### Text, anchors and counts

- The text of a block is its lines joined with newlines, **without** the final
  newline. `old` / `new` in the examples below are written as one line each.
- `replace` and the anchors of `insert-*` match **exactly** as substrings
  (case, whitespace and all). There is no regex and no fuzzy matching.
- A hunk must match **exactly `count` times** (default 1). `count=3` demands
  three matches; `count=all` replaces every match but needs at least one.
  Fewer or more matches fail the whole run, which is the point: a drifted
  anchor can never silently do nothing.
- Because the text has no final newline, a partial line works as you would
  hope: `timeout: 30` matches inside `  timeout: 30 # seconds`. To include a
  newline, end the block with an empty line:

  ```text
  === replace
  debug = true

  === with
  === end
  ```

  The old text is `debug = true` plus a newline, so the whole line goes; an
  empty `with` deletes.
- Every hunk is matched against the file **as it was before the run**, not
  against the result of earlier hunks. Line numbers in messages therefore
  always refer to the file on disk, and hunk order does not matter. Two hunks
  that touch overlapping text are rejected (`overlap`), not guessed at.

### insert-after and insert-before

These work on whole lines. The inserted text is added after the line that
contains the end of the anchor (or before the line that contains its start).
A single empty line in the block inserts a blank line.

```text
=== insert-after
[server]
=== with
host = example.org
=== end
```

If the anchor is on the last line and the file has no final newline, the
inserted line also gets no final newline, so the file keeps ending the way it
did.

### append

Adds the lines at the end of the file, supplying the newline the last line was
missing if needed. The file always ends with a newline afterwards.

### upsert-section

For Markdown files (and anything with ATX headings). The section starts at the
heading line and runs to the next heading of the same or a higher level, or
the end of the file. Sub-sections belong to the section and are replaced with
it.

```text
=== upsert-section "## Install"

Run `go install github.com/baditaflorin/fpatch/cmd/fpatch@latest`.
=== end
```

- The heading is matched by level and text (closing `#`s are ignored); the
  existing heading line is kept as it is.
- The body is taken verbatim. Start it with an empty line if you want a blank
  line after the heading, as above. Trailing blank lines of the existing
  section are kept, so spacing before the next heading does not change.
- Headings inside fenced code blocks (` ``` ` or `~~~`) are not headings.
- If the heading does not exist, the section is added at the end of the file
  after a blank line. If it exists twice, that is an error (exit 1): fpatch
  will not guess which one you meant.
- Setext headings (underlined with `===` or `---`) are not recognised.
- If the section already has exactly this content, the hunk is reported as
  `already-applied` and changes nothing.

### Several files

With no `=== file` lines, every hunk applies to every file named on the command
line, and each file must match each hunk. With `=== file PATH` lines, every
hunk must sit under one, and `PATH` has to be **exactly one of the FILE
arguments as you typed it**: the spec can never introduce a path the command
line did not show. A file named on the command line that no `=== file` line
mentions is left alone.

### Line endings, BOM, final newline

- A file whose every line ends in CRLF is matched as LF and written back as
  CRLF, so one spec works for both; diffs show LF.
- A UTF-8 byte order mark is kept.
- A missing final newline stays missing (except after `append` and `upsert-section`
  when they add a section, which always end with one).
- A file with mixed LF and CRLF endings is matched byte for byte; if a
  multi-line anchor misses there, the message says so.
- Specs may themselves use CRLF; that is normalised.
- Binary files (any NUL byte) are refused.

### When the spec contains `=== end`

If your content needs a line that looks like a directive, pick another marker:

```sh
fpatch --marker '@@@' docs.md <<'EOF'
@@@ replace
=== end
@@@ with
=== stop
@@@ end
EOF
```

A directive keyword that shows up inside a block (say, a forgotten `=== end`
followed by the next `=== replace`) is reported as an error rather than
swallowed as text.

## Flags

| Flag | Meaning |
| --- | --- |
| `--host ALIAS` | Patch the files on a remote host over ssh. `ALIAS` is a host name or `~/.ssh/config` alias. An empty value is an error, never "local". |
| `--fs TARGET` | Any registered file system, `name` or `name:arg` (`--host web1` is `--fs ssh:web1`). Default `local`. |
| `-n`, `--dry-run` | Compute and show everything; write nothing. |
| `--json` | One JSON object per file, then a summary object ([schema](#json-output)). |
| `--backup` | Keep the previous content of each changed file as `FILE.bak` (same mode). An existing `FILE.bak` is replaced (a symlink at that path is replaced too, not followed). |
| `--idempotent` | A hunk whose effect is already present counts as success instead of a miss. |
| `-q`, `--quiet` | Do not print diffs (also leaves them out of `--json`), for files whose content should not reach a log. |
| `-f`, `--spec FILE` | Read the spec from `FILE` instead of stdin (`-` is stdin). |
| `--marker STR` | Directive marker instead of `===`. |
| `--timeout DUR` | Give up after `DUR` (default `2m`; `0` for no limit). |
| `--version`, `-h` | Print the version number / the help. |

Flags may come before, between or after the file names. `--` ends the flags, so
a file called `-n` is `fpatch -- -n`.

`--idempotent` details: a `replace` counts as applied when the old text is gone
and the new text is there (when the new text contains the old one, as in
`timeout: 30` becoming `timeout: 30 # seconds`, it counts as applied when the
new text is already there at least `count` times); a deletion when the old text
is gone; an `insert-*` when the lines already sit where they would go; an
`append` when the file already ends with those lines. This is a heuristic by
nature; run with `--dry-run` if in doubt.

fpatch never creates files (a mistyped path should be an error, not a new
file) and never removes them.

## Exit codes

| Code | Meaning | Written? |
| --- | --- | --- |
| `0` | Applied. With `--dry-run`: changes computed. | yes (no with `--dry-run`) |
| `1` | An anchor is missing or ambiguous (or the count is off, or hunks overlap, or a section heading is not unique). | no |
| `2` | Usage or spec error: bad flag, bad spec, spec names a file that is not an argument, bad host alias. | no |
| `3` | I/O error: file missing or unreadable, binary file, permission denied, ssh failure, timeout, or a file changed during the run. | no, or rolled back (see below) |
| `4` | Nothing to do: the spec is valid but every hunk is already applied or changes nothing. | no |

If a run has both an I/O error and an anchor miss, the exit code is `3`.
Scripts that treat "already applied" as success should accept `0` and `4`:
`fpatch --idempotent ... || [ $? -eq 4 ]`.

## What a miss looks like

On a miss fpatch prints the most similar block of the file (line-based
similarity, standard library only) and cheap hints, to stderr:

```text
fpatch: config.yml: hunk 1 (replace, spec line 1): anchor not found (expected 1 match)
  nearest match: lines 3-4, similarity 1.00:
    3 |   port: 8080
    4 |   timeout: 30
  hint: the anchor matches when runs of whitespace are ignored: check indentation, tabs against spaces, and trailing spaces
fpatch: nothing was written: 1 hunk did not match as demanded
```

An ambiguous anchor lists where it matched:

```text
fpatch: config.yml: hunk 1 (replace, spec line 1): anchor matches 2 times (expected 1); make it longer so it is unique, or set count=2 / count=all
  matches start at line 12, 40
```

## JSON output

`--json` prints [JSON Lines](https://jsonlines.org/): one object per file, in
the order given, then one summary object. Nothing else goes to stdout, and
stderr stays empty (apart from usage errors, which are also reported in the
summary).

File object:

| Field | Type | Meaning |
| --- | --- | --- |
| `type` | string | `"file"` |
| `file` | string | The path as given on the command line. |
| `status` | string | `applied`, `would-apply` (dry run), `unchanged`, `failed`, `not-written` (fine by itself, but the run stopped because of another file), `rolled-back`, `left-modified` (written, and restoring it failed). |
| `changed` | bool | The patched content differs from the file. |
| `written` | bool | The file on disk now holds the patched content. |
| `line_ending` | string | `lf`, `crlf` or `mixed` (omitted when the file was not read). |
| `hunks` | array | One object per hunk that applies to this file (below). |
| `diff` | string | Unified diff (omitted when unchanged or with `--quiet`). |
| `backup` | string | Path of the backup, with `--backup`. |
| `error` | string | Why the file failed, when it did for a reason other than a hunk. |

Hunk object:

| Field | Type | Meaning |
| --- | --- | --- |
| `index` | number | 1-based position of the hunk in the spec. |
| `op` | string | `replace`, `insert-after`, `insert-before`, `append`, `upsert-section`. |
| `spec_line` | number | Line of the hunk in the spec. |
| `status` | string | `applied`, `already-applied`, `no-op`, `missing`, `ambiguous`, `count-mismatch`, `overlap`. |
| `matches` | number | How many times the anchor matched. |
| `expected` | number | How many it had to match (`0` for `count=all` and for `append`). |
| `all` | bool | `count=all` (omitted otherwise). |
| `lines` | number[] | Line numbers of the first 20 matches, in the file as it is on disk. |
| `nearest` | object | On a miss: `start_line`, `end_line`, `similarity` (0 to 1), `lines` (the text). |
| `hints` | string[] | Cheap explanations of a miss. |
| `message` | string | Human-readable explanation. |

Summary object: `type` (`"summary"`), `exit_code`, `dry_run`, `files`,
`changed` and `written` (counts of files), `notes` (what a rollback did) and
`error` (set for usage errors).

```json
{"type":"file","file":"app.conf","status":"applied","changed":true,"written":true,"line_ending":"lf","hunks":[{"index":1,"op":"replace","spec_line":1,"status":"applied","matches":1,"expected":1,"lines":[3]}],"diff":"--- app.conf\n+++ app.conf\n@@ -2,3 +2,3 @@\n..."}
{"type":"summary","exit_code":0,"dry_run":false,"files":1,"changed":1,"written":1}
```

## Remote hosts

`--host ALIAS` runs the same patch on a remote machine. fpatch starts the
system `ssh` binary, so your `~/.ssh/config`, ProxyJump and ControlMaster
settings apply, and **nothing is installed on the remote side**.

- **One connection per run.** A single `ssh ALIAS sh` session carries every
  read and write; it is not one connection per file.
- Files are read with `cat` and replaced with a temporary file in the same
  directory plus `mv`, keeping the mode (and the owner when permitted).
- Relative paths are relative to the remote login directory (usually the home
  directory); use absolute paths to be sure.
- Paths come only from the command line and are validated: control characters
  are refused, the alias cannot start with `-`, and paths are passed to the
  remote shell as quoted data. The spec on stdin cannot add paths.
- ssh runs with `BatchMode=yes` (no password prompts, so it cannot hang an
  agent), `ClearAllForwardings=yes` and `RemoteCommand=none`.
  Unknown host keys fail instead of prompting; fpatch never weakens host key
  checking.

Needs on the remote host: a POSIX `sh` reachable as `sh`, and the usual
tools `cat cksum chmod chown cp dirname head mktemp mv readlink rm stat tr
wc` (GNU/Linux, busybox and BSD/macOS variants are handled). The ssh client
needs OpenSSH 7.6 or newer for `RemoteCommand=none`.

What it can and cannot do is spelled out in [SECURITY.md](SECURITY.md).

## What "atomic" means here

- **Check everything, then write.** Every file is read and every hunk is
  checked before the first byte is written. Any miss, ambiguity, overlap,
  unreadable or binary file stops the run with nothing written (exit 1 or 3).
- **Each file is replaced atomically**: the new content is written to a
  temporary file next to it and renamed over it, so a reader sees the old or
  the new file, never a mix, and a failed write leaves the original and no
  stray temporary file.
- **A file that changed while fpatch worked is not overwritten.** Just before
  writing a file, fpatch compares it with what it read (a content hash
  locally, POSIX `cksum` over ssh). A mismatch is exit 3. A small window
  between that check and the rename remains.
- **Several files are not a database transaction.** Files are replaced one
  after the other. If a write fails part-way (disk full, permission, lost
  connection), the files already replaced are put back from the copies held in
  memory, and the report says what happened to each. If a restore also fails,
  the report says which file is left patched (exit 3). Over ssh, if the
  connection drops during a write, fpatch reconnects once to read the file back
  and undo the run. A process killed between
  two renames, or a power cut, can leave some files patched and some not;
  nothing short of a journal prevents that.
- Symlinks are followed: the target gets the new content and the link stays a
  link. Hard links are not preserved (the rename gives the file a new inode).

## When to use fpatch vs an editor tool or sed

fpatch is not a better editor tool. It does a different job, and where an
editor tool fits, use that.

**Claude Code's built-in Edit tool** (and similar `str_replace` tools in other
agents) is local, edits one file per call, and already fails loudly when the
text is not found or is not unique. For a single edit to a local file in an
interactive session, use it. It needs no spec, and it is built into the agent.

What fpatch adds:

- **Remote hosts**: patch a file on another machine over your existing ssh
  setup, with no remote install and one connection.
- **Several files, all or nothing**: one run that cannot leave half of a
  coordinated change applied, with a rollback if a write fails.
- **Section upsert**: replace or add a Markdown section by heading, without
  anchoring on its old text.
- **A scriptable CLI** with stable exit codes and `--json`, for CI jobs and for
  agents or scripts that have no edit tool, or run in a shell only.
- **Idempotent re-runs** (`--idempotent`), so a provisioning script can apply
  the same edit repeatedly.
- **Nearest-match diagnostics** that tell the caller how to fix a drifted
  anchor, instead of only "not found".
- **A portable `sed -i`**: GNU `sed -i 's/a/b/'` and BSD/macOS `sed -i '' 's/a/b/'`
  are incompatible, and neither refuses to silently do nothing when the
  pattern is absent. fpatch behaves the same everywhere and exits `1` on a
  miss.
- **No quoting**: the spec is read verbatim from a heredoc, so code with
  quotes, backslashes and `$` needs no escaping, unlike `python -c`, `sed` or
  `perl -pe` one-liners.

When not to use it:

- A pattern-based change across many places: use `sed`, `perl -pi`, `sd` or
  `ast-grep`. fpatch has no regular expressions and no fuzzy matching, by
  design.
- Refactors that need to understand code (renames, imports): use a
  language-aware tool.
- Creating, moving or deleting files, or changing binary files: use the shell.
- One quick edit to a local file when your tool already has an edit command.

## Limitations

- Matching is exact and literal. There is no regex.
- Ownership is preserved only when permitted (as root, or when you own the
  file); ACLs, extended attributes and SELinux labels are **not** preserved,
  because the file is replaced by a new one. On a host with SELinux enforcing,
  check the label of a patched system file (`restorecon`).
- Hard links are broken by the replacement.
- Files above 64 MiB are refused.
- Over ssh, fpatch needs the standard tools listed above and a login that
  allows running `sh`. A login shell that prints to stdout is tolerated;
  forced commands (`ForceCommand`) are not supported.
- Windows is not a target. It should build, but is not tested.
- The test suite runs the ssh backend against GNU/Linux coreutils through a
  stand-in for the `ssh` binary; the remote helper script was also exercised
  by hand under macOS's own `sh`, `dash` and `bash`. A real network ssh path
  is not part of the automated tests.

## Development

```sh
make check        # gofmt check, go vet, tests
make race         # tests with the race detector
make fuzz         # fuzz the spec parser, the matcher, the diff and the quoting
make dist         # archives and checksums.txt in dist/
```

The tests include fuzzing seeds, atomicity and rollback tests, a contract suite
that every file-system backend must pass, and end-to-end runs of the built
binary. See [CONTRIBUTING.md](CONTRIBUTING.md) and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Licence

MIT, see [LICENSE](LICENSE).
