# Changelog

All notable changes to fpatch are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/). The spec format and the exit
codes are the public interface; so is the `fsys.FS` contract.

## [Unreleased]

## [0.1.0] - not yet released

First version. Prepared but not tagged: the tag is created after review.

### Added

- `fpatch FILE... < spec`: exact-match, all-or-nothing patching driven by a
  heredoc-friendly spec on stdin.
- Hunks: `replace` (with `count=N|all`), `insert-after`, `insert-before`,
  `append`, `upsert-section "## Heading"`, and `=== file PATH` scopes for
  per-file hunks in one all-or-nothing run.
- Every hunk must match exactly `count` times or nothing is written, in any
  file. Hunks are matched against the file as it was before the run; overlap
  is an error.
- On a miss: the nearest matching block with line numbers and hints (case,
  whitespace, mixed line endings).
- Unified diff output, `--dry-run`, `--json` (JSON Lines), `--quiet`,
  `--backup`, `--idempotent`, `--marker`, `--spec`, `--timeout`, `--version`.
- Atomic writes (temporary file in the same directory plus rename); mode,
  owner where permitted, symlinks, CRLF, BOM and the final newline are
  preserved; binary files are refused; a file changed during the run is not
  overwritten; a failed write rolls back the files already replaced.
- Exit codes: 0 applied, 1 anchor missing or ambiguous, 2 usage or spec error,
  3 I/O error, 4 nothing to do.
- Pluggable file systems behind `fsys.FS` with a registry (`fsys.Register`,
  `fsys.New`): `local`, and `ssh:ALIAS` (`--host ALIAS`), which uses the system
  `ssh` binary, one connection per run, and needs nothing installed remotely.
  A contract test suite (`fsys/fsystest`) and an in-memory backend
  (`fsys/memfs`) are included.
- Documentation: README, `docs/ARCHITECTURE.md`, `CONTRIBUTING.md`,
  `SECURITY.md`; a `Makefile` (`make dist` cross-builds darwin and linux for
  amd64 and arm64 with checksums); a Woodpecker CI configuration.
