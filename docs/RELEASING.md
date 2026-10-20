# Releasing fpatch

A release is a GitHub Release of [baditaflorin/fpatch](https://github.com/baditaflorin/fpatch/releases). It carries six files:

| File | What it is |
|---|---|
| `fpatch_<version>_darwin_arm64.tar.gz`, `..._darwin_amd64.tar.gz`, `..._linux_amd64.tar.gz`, `..._linux_arm64.tar.gz` | the static binary plus its documentation, built by `make dist` |
| `checksums.txt` | `<sha256>  <archive name>`, one line per archive |
| `install.sh` | the installer, from the commit that was released |

`scripts/release.sh` builds these files and, only when you ask it to, publishes them. The version in the
archive names has no leading `v`; the tag has one (`v0.1.0` builds `fpatch_0.1.0_...`), and
`fpatch --version` prints the bare version.

## Before you start

- You are in a clone of `baditaflorin/fpatch`, on an up-to-date `main`, with a clean working tree. The script refuses
  otherwise: `HEAD` must be `origin/main`.
- `CHANGELOG.md`: the newest section is the version you are releasing, its entries moved out of
  `Unreleased` and its heading dated.
- The tag does not exist yet, locally or on `origin`. The script never tags by hand: `gh release create`
  creates `vX.Y.Z` at the exact commit that was built.
- `BUILD_HOST` names a Linux machine you can reach with `ssh` and where your user can run `docker`
  (an ssh alias or `user@host`). There is deliberately no default, so a release is never built on a machine
  by accident. Builds do not happen on a laptop: that keeps the toolchain and base image the same every time.
- For `--publish` only: `gh` is installed and logged in with the right to create releases here.

## Dry run (the default)

```sh
BUILD_HOST=my-build-host scripts/release.sh v0.1.0
```

It does these things, in this order, and stops at the first problem:

1. **Guards**, all local, before anything is built: `origin` is `baditaflorin/fpatch`; the tree is clean; `HEAD` is
   `origin/main`; the tag does not exist; `CHANGELOG.md` has a section for the version, nothing is left
   under `Unreleased`. A heading that still says "unreleased" is a warning in a dry run
   and a refusal with `--publish`.
2. **The build host**: one `ssh` that prints its load, core count and free space.
3. **The build**: `git archive HEAD` goes to the host over one `ssh` connection, `make dist VERSION=v0.1.0`
   runs inside `docker run --rm --init golang:1.26` (named volumes keep the Go caches between runs), and
   `dist/` comes back on the same connection as a tar stream. The container gives `dist/` back to the host
   user before it exits, so the host's temporary directory can be removed. No token and no repository
   history reach the host.
4. **Verification, here**: every archive's sha256 matches `dist/checksums.txt`; there are exactly four
   archives, one per platform, and no unlisted ones; each holds the binary, `README.md` and `LICENSE`; the binary for this
   machine runs and prints exactly the bare version.
5. **The plan**: every file that `--publish` would upload with its size and sha256, the release notes
   (the CHANGELOG section of the version), and the exact `gh release create` command.

Nothing is uploaded and no tag is created. `dist/` stays in the working tree (it is git-ignored), which is
also what `./install.sh --from-dir dist` and `scripts/test-install.sh dist` use.

The archives are reproducible: building the same commit twice with the same Go image gives the same
`checksums.txt`. If two builds differ, check whether the `golang` image moved to a newer patch release
between them; the checksums that matter are the ones of the release you publish.

## Publish

```sh
BUILD_HOST=my-build-host scripts/release.sh v0.1.0 --publish
```

This repeats the whole dry run (a publish never reuses an older `dist/`), then asks you to type the tag
and runs, from your machine:

```sh
gh release create v0.1.0 --repo baditaflorin/fpatch --target <the built commit> --title "fpatch v0.1.0" \
  --notes-file <the CHANGELOG section> dist/fpatch_0.1.0_*.tar.gz dist/checksums.txt install.sh
```

Because that runs on your machine, no GitHub token ever sits on the build host. `--yes` skips the question
for scripted use. It refuses when the release already exists or `gh` is not logged in.

Afterwards:

```sh
gh release view v0.1.0 --repo baditaflorin/fpatch                 # six assets
./install.sh --dry-run --prefix "$(mktemp -d)"    # the published release resolves and verifies
```

## Trust: `checksums.txt` is the only anchor

`install.sh`, and anything built on the same archives later (a package manager formula, say), trusts one
thing: the `checksums.txt` of the release it downloads from. The installer fetches the archive and that
file from one release, computes the archive's sha256 and compares it with the line for that archive. A
mismatch, a missing line, a duplicated line and a malformed line are all refusals, and a refusal leaves
the previously installed binary untouched.

That catches a truncated or corrupted download and an archive that was swapped or edited while
`checksums.txt` was not. It does **not** help against someone who can replace both files in the release:
that needs write access to this repository's releases, so keep that account
protected (two-factor authentication) and publish only from your own machine, which is why the
build host holds no token. There are no signatures and no transparency log. `install.sh` itself is not
covered by `checksums.txt` either: you run it before anything is verified, so read it (it is one file) or
take it from a checkout you trust. For more assurance, build the same tag yourself on another host and
compare the `checksums.txt` files.

## Testing the scripts

- `scripts/test-install.sh [dist]` runs `install.sh` against a real `dist/` in a scratch `HOME` and `TMPDIR`
  (fresh install, re-run, `--status`, `--dry-run`, `--prefix`, a tampered archive, a tampered `checksums.txt`,
  a missing asset, an unsupported platform, `--uninstall`, and the download branches through fake `gh` and curl
  commands). Use `BASH_BIN=/bin/bash /bin/bash scripts/test-install.sh dist` to run it under macOS's bash 3.2.
- `scripts/test-release.sh` runs `release.sh` against a local bare "origin" with a fake `ssh` that plays the
  build host and a fake `gh`, on a `PATH` that has no real `ssh`, `gh` or `docker`. It checks every guard, the
  verification of what comes back, the dry run, and what `--publish` would run. Nothing real is reached.

CI runs both (`.woodpecker.yml`) after running `shellcheck` over `install.sh`, `scripts/release.sh`, `scripts/test-install.sh`
and `scripts/test-release.sh`.

## When something goes wrong

- **The build failed**: nothing was uploaded. Fix it and run the dry run again. The temporary directory on
  the host is removed by the script; the Go cache volumes stay on purpose.
- **The host is busy or short of disk**: the script warns or refuses and says so. Pick another
  `BUILD_HOST`.
- **A bad release went out**: delete it (`gh release delete vX.Y.Z --cleanup-tag`) and publish a new patch
  version. Do not reuse a version number: someone may already have downloaded it, and its checksums are
  the only thing that told them what they have.
