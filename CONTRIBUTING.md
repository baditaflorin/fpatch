# Contributing

Thanks for looking. fpatch is deliberately small; the best contributions keep
it that way.

## Ground rules

- **Standard library only.** A new dependency needs a very good reason and an
  entry in the decision record in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
- **The Go floor is 1.22** (`go.mod`). Do not use newer language or library
  features; CI builds on Go 1.22 and on the latest release.
- **All-or-nothing is the product.** A change that can write to a file after a
  hunk or a file has failed is a bug, whatever else it does.
- **Paths come only from the command line.** The spec is data; it may select
  among the file arguments but never add a path.
- **No regular expressions, no file creation, no deletion.** Those are
  deliberate non-features (see the decision record). Open an issue before
  starting work on one.
- **Never interpolate a user-supplied value into shell code.** The ssh backend
  passes quoted arguments and quoted here-documents only; keep it that way and
  add a test with a hostile value for anything new.

## Setup and checks

You need Go 1.22 or newer and a POSIX shell.

```sh
make check      # gofmt check, go vet, tests
make race       # tests with the race detector
make fuzz       # fuzz targets, FUZZTIME=30s each (override with FUZZTIME=2m)
```

The end-to-end tests under `cmd/fpatch` build the binary with `go build`, so
`go` must be on your `PATH` when you run the tests.

Run the suite once as a normal user as well as, if you use containers, as
root: some permission tests skip as root because root ignores file modes.

Heavy work (full test runs, fuzzing, cross-builds) belongs on a build machine,
not necessarily your laptop; it is plain `go test` and fits in a container:

```sh
docker run --rm --init -v "$PWD":/src -w /src golang:1.22 go test ./...
docker run --rm --init -v "$PWD":/src -w /src golang:1.26 go test -race ./...
```

(`--init` matters: the ssh tests start many short-lived processes.)

## Making a change

1. Write the test first when you can. Table tests in the package you touch are
   the house style. A bug fix needs a test that fails without it.
2. Keep functions short and comments about *why*. Match the surrounding style;
   run `make fmt`.
3. Update the README (user-visible behaviour), `docs/ARCHITECTURE.md` (design),
   and `CHANGELOG.md` (under `Unreleased`) in the same change.
4. If the spec grammar changes, extend the fuzz seeds in
   `internal/spec/parse_test.go` and the round trip in `Format`.
5. Open a pull request against `main`. Describe what you verified and what you
   did not.

### Adding a file-system backend

See "Adding a backend" in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). The
short version: implement `fsys.FS`, register it with `fsys.Register`, and make
it pass `fsystest.Run`. A backend that cannot pass the contract is not ready.

### Changing the ssh helper

`fsys/ssh_prelude.sh` runs on other people's machines. Keep it POSIX `sh`, use
only the tools listed in the README, do not introduce `local`, arrays, `[[`,
`echo -e`, `printf %q` or GNU-only flags, and check any change under
`dash`, `bash` and, if you can, a BSD/macOS `sh`. Source it with a token set
(`FPT=x . fsys/ssh_prelude.sh`) and call `fp_read`/`fp_write` by hand.

## Releasing

Releases are cut by a maintainer after the change is reviewed and merged.

1. Move the `Unreleased` entries of `CHANGELOG.md` under the new version and
   date, and commit.
2. `make check race` on a clean `main`, then `make dist VERSION=vX.Y.Z` and look
   at `dist/SHA256SUMS`.
3. Tag: `git tag -a vX.Y.Z -m "fpatch vX.Y.Z" && git push origin vX.Y.Z`.
   `go install github.com/baditaflorin/fpatch/cmd/fpatch@vX.Y.Z` works from the
   tag alone.
4. If you publish a GitHub release, attach the contents of `dist/` including
   `SHA256SUMS`.

Nothing in the repository publishes or tags anything automatically.

## Conduct

Be kind and be specific. Assume good faith.

## Licence

By contributing you agree that your work is released under the MIT licence of
this repository.
