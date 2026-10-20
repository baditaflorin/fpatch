#!/usr/bin/env bash
# Tests for scripts/release.sh. Nothing real is reached: the repository under test is cloned from a local
# bare "origin", ssh and gh are fakes on a PATH that holds no real ssh, gh or docker, and the "build host"
# is a fake ssh that hands back a prepared dist/. So no build host, no network and no GitHub release is
# involved, and --publish can only ever reach the fake gh.
#
#   scripts/test-release.sh
#
# It tests the COMMITTED state of this repository (git HEAD), not uncommitted edits.
# Works with bash 3.2. Exit 0 when everything passed.
set -u

TOOL=fpatch
REPO=baditaflorin/fpatch
MEMBERS="fpatch README.md LICENSE"
HAS_VERSION_FILE=0
HOSTNAME_FOR_TESTS=test-build-host

BASH_BIN=$(command -v "${BASH_BIN:-bash}") || { echo "bash not found" >&2; exit 2; }
here=$(cd "$(dirname "$0")/.." && pwd)
git -C "$here" rev-parse --verify HEAD >/dev/null 2>&1 || { echo "test-release: $here is not a git repository with a commit" >&2; exit 2; }

T=$(mktemp -d "${TMPDIR:-/tmp}/$TOOL-reltest.XXXXXX") || exit 2
trap 'chmod -R u+rwx "$T" 2>/dev/null; rm -rf "$T"' EXIT
export HOME=$T/home TMPDIR=$T/tmp GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
mkdir -p "$HOME" "$TMPDIR" "$T/shims" "$T/fixture"

# ---- harness ---------------------------------------------------------------------------------------

n_pass=0
n_fail=0
out=
rc=0
section() { printf '\n-- %s\n' "$1"; }
pass() { n_pass=$((n_pass + 1)); printf 'ok   %s\n' "$1"; }
fail() {
  n_fail=$((n_fail + 1)); printf 'FAIL %s\n' "$1"
  if [ -n "$out" ]; then printf '%s\n' "$out" | sed 's/^/       | /'; fi
}
rc_is() { if [ "$rc" -eq "$1" ]; then pass "$2"; else fail "$2 (exit status $rc, wanted $1)"; fi; }
says() { case $out in *"$1"*) pass "$2" ;; *) fail "$2 (output lacks: $1)" ;; esac; }
silent_on() { case $out in *"$1"*) fail "$2 (output has: $1)" ;; *) pass "$2" ;; esac; }
check() { local name=$1; shift; if "$@"; then pass "$name"; else fail "$name"; fi; }
is_true() { if "$@"; then return 0; else return 1; fi; }
# the tree the fake build host received: a tar of the committed files, with no .git
is_tar_without_git() {
  tar -tf "$FAKE_SSH_TREE" >"$T/tree.list" 2>/dev/null || return 1
  grep -q 'release.sh$' "$T/tree.list" && grep -q 'install.sh$' "$T/tree.list" || return 1
  ! grep -q '\.git/' "$T/tree.list"
}
calls() { if [ -f "$1" ]; then wc -l <"$1" | tr -d ' '; else echo 0; fi; }

hash_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{ print $1 }'; else shasum -a 256 "$1" | awk '{ print $1 }'; fi
}

# ---- a PATH that cannot reach a real ssh, gh or docker ---------------------------------------------

mkmin() {
  local d=$1 t p
  shift
  mkdir -p "$d"
  for t in "$@"; do
    p=$(command -v "$t" 2>/dev/null) || continue
    case $p in /*) ln -s "$p" "$d/$t" ;; esac
  done
}
mkmin "$T/bin" git awk sed tar gzip mktemp find wc tr grep sort uname id dirname basename cat rm mv mkdir chmod cp ls head tail \
  sleep date sha256sum shasum cmp od dd readlink env sh bash
check "the test PATH has no real ssh, gh or docker" test -z "$(PATH=$T/bin command -v ssh gh docker 2>/dev/null)"

# fake ssh: logs every call; the status call answers from $FAKE_SSH_STATUS; the build call swallows the
# tree on stdin and answers with the tar stream of the prepared dist in $FAKE_DIST (or fails)
cat >"$T/shims/ssh" <<'SHIM'
#!/bin/sh
printf 'ssh %s\n' "$(printf '%s' "$*" | tr '\n' ' ')" >>"$FAKE_SSH_LOG"
[ "$1" = -o ] && shift 2
host=$1
shift
case "$*" in
  "uptime; nproc; df -Pk /tmp | tail -n 1")
    [ "${FAKE_SSH_UNREACHABLE:-0}" = 0 ] || { echo "ssh: Could not resolve hostname $host" >&2; exit 255; }
    cat "$FAKE_SSH_STATUS" ;;
  "bash -c "*)
    cat >"$FAKE_SSH_TREE"
    [ "${FAKE_SSH_BUILD:-ok}" = ok ] || { echo "docker: simulated build failure" >&2; exit 1; }
    tar -c -C "$FAKE_DIST" . ;;
  *) echo "fake ssh: unexpected command: $*" >&2; exit 99 ;;
esac
SHIM
# fake gh: logs every call; `release view` fails (no release yet) unless $FAKE_GH_EXISTS=1;
# `release create` keeps a copy of the notes file
cat >"$T/shims/gh" <<'SHIM'
#!/bin/sh
echo "gh $*" >>"$FAKE_GH_LOG"
case "$1 $2" in
  "auth status") [ "${FAKE_GH_AUTH:-1}" = 1 ] && exit 0; echo "not logged in" >&2; exit 1 ;;
  "release view") [ "${FAKE_GH_EXISTS:-0}" = 1 ] && exit 0; exit 1 ;;
  "release create")
    while [ $# -gt 0 ]; do
      if [ "$1" = --notes-file ]; then cp "$2" "$FAKE_GH_NOTES"; fi
      shift
    done
    exit 0 ;;
esac
echo "fake gh: unexpected: $*" >&2
exit 99
SHIM
chmod 755 "$T"/shims/*
export FAKE_SSH_LOG=$T/ssh.log FAKE_GH_LOG=$T/gh.log FAKE_SSH_TREE=$T/tree.tar FAKE_GH_NOTES=$T/notes.copy
export FAKE_SSH_STATUS=$T/status.txt FAKE_DIST=$T/fake-dist
status_ok() {
  cat >"$FAKE_SSH_STATUS" <<'STATUS'
 07:54:19 up 2 days,  3:09, 11 users,  load average: 6.00, 6.50, 7.00
20
tmpfs           6042124 1726552   4315572      29% /tmp
STATUS
}
status_ok
rel_path=$T/shims:$T/bin

# mk_dist DIR VERSION [lie]: four archives and a checksums.txt; the binary is a script that prints VERSION
# (or "lie" instead when given)
mk_dist() {
  local d=$1 v=$2 lie=${3:-$2} p m
  rm -rf "$d"
  mkdir -p "$d"
  for p in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
    mkdir "$d/stage"
    for m in $MEMBERS; do
      if [ "$m" = "$TOOL" ]; then printf '#!/bin/sh\necho %s\n' "$lie" >"$d/stage/$m"; chmod 755 "$d/stage/$m"; else echo "doc" >"$d/stage/$m"; fi
    done
    # shellcheck disable=SC2086  # MEMBERS is a list of file names without spaces
    (cd "$d/stage" && tar -cf - $MEMBERS | gzip -n >"../${TOOL}_${v}_${p}.tar.gz")
    rm -rf "$d/stage"
  done
  (cd "$d" && for f in "${TOOL}_${v}_"*.tar.gz; do printf '%s  %s\n' "$(hash_of "$f")" "$f"; done >checksums.txt)
}

# ---- the repository under test, as a clone of a local bare origin -----------------------------------

bare=$T/origins/baditaflorin/$(basename "$REPO").git
mkdir -p "$(dirname "$bare")"
git init -q --bare -b main "$bare"
git -C "$here" push -q "$bare" HEAD:refs/heads/main || { echo "test-release: cannot copy HEAD into the fixture origin" >&2; exit 2; }
fix=$T/fixture/work
git clone -q "$bare" "$fix"
gitc() { git -C "$fix" -c user.name=test -c user.email=test@example.invalid "$@"; }
# a known CHANGELOG, so the guards have something predictable to read
write_changelog() { # write_changelog HEADING_SUFFIX
  printf '# Changelog\n\n## [Unreleased]\n\n## [0.1.0]%s\n\n### Added\n\n- first release notes line\n- second line\n' "$1" >"$fix/CHANGELOG.md"
}
write_changelog " - 2026-11-01"
gitc add CHANGELOG.md
gitc commit -q -m "test changelog"
gitc push -q origin main
base_head=$(git -C "$fix" rev-parse HEAD)
reset_fixture() {
  gitc checkout -q main 2>/dev/null
  gitc reset -q --hard "$base_head"
  gitc clean -q -fdx
  gitc tag -d v0.1.0 >/dev/null 2>&1
  git -C "$bare" tag -d v0.1.0 >/dev/null 2>&1
  gitc push -q -f origin "$base_head:refs/heads/main"
  gitc remote set-url origin "$bare"
  : >"$FAKE_SSH_LOG"
  : >"$FAKE_GH_LOG"
  status_ok
}
rel() { out=$(cd "$fix" && PATH=$rel_path BUILD_HOST=${BUILD_HOST-$HOSTNAME_FOR_TESTS} "$BASH_BIN" scripts/release.sh "$@" 2>&1 </dev/null); rc=$?; }
refused_early() { # refused_early NAME: refused, exit 1, and no ssh was attempted
  rc_is 1 "$1"
  check "  and nothing was started: no ssh call" test "$(calls "$FAKE_SSH_LOG")" -eq 0
}
good_dist() { mk_dist "$FAKE_DIST" 0.1.0; }
good_dist

printf 'bash: %s\nrepository under test: %s (HEAD %s)\n' "$("$BASH_BIN" --version | head -n 1)" "$here" "$(git -C "$here" rev-parse --short HEAD)"

# ---------------------------------------------------------------------------------------------------
section "usage"
reset_fixture
rel
rc_is 2 "no version given is a usage error"
rel 0.1.0
rc_is 2 "a version without the leading v is a usage error"
rel v0.1
rc_is 2 "a malformed version is a usage error"
rel v0.1.0 --bogus
rc_is 2 "an unknown option is a usage error"
rel v0.1.0 --yes
rc_is 2 "--yes without --publish is a usage error"
rel v0.1.0 v0.2.0
rc_is 2 "two versions are a usage error"
check "  none of them started anything" test "$(calls "$FAKE_SSH_LOG")" -eq 0
BUILD_HOST='' rel v0.1.0
rc_is 1 "an empty BUILD_HOST is refused (there is deliberately no default)"
says "BUILD_HOST is not set" "  with a clear message"
BUILD_HOST='bad host;rm' rel v0.1.0
rc_is 1 "a BUILD_HOST with odd characters is refused"
check "  nothing was started" test "$(calls "$FAKE_SSH_LOG")" -eq 0

# ---------------------------------------------------------------------------------------------------
section "guards: each refusal happens before anything is built"
reset_fixture
echo stray >"$fix/stray.txt"
rel v0.1.0
refused_early "an untracked file makes the tree dirty"
says "not clean" "  and says so"
reset_fixture
echo "# edited" >>"$fix/CHANGELOG.md"
rel v0.1.0
refused_early "an edited tracked file makes the tree dirty"
reset_fixture
echo more >>"$fix/CHANGELOG.md"
gitc commit -q -am "local only"
rel v0.1.0
refused_early "a local commit that is not on origin/main is refused"
says "is not origin/main" "  and says so"
reset_fixture
(cd "$T/fixture" && rm -rf other && git clone -q "$bare" other && cd other && echo x >x.txt && git add x.txt && git -c user.name=t -c user.email=t@example.invalid commit -q -m "newer on origin" && git push -q origin main)
rel v0.1.0
refused_early "a clone that is behind origin/main is refused"
reset_fixture
gitc tag v0.1.0
rel v0.1.0
refused_early "a tag that exists locally is refused"
says "already exists locally" "  and says so"
reset_fixture
git -C "$bare" tag v0.1.0 "$base_head"
rel v0.1.0
refused_early "a tag that exists on origin is refused"
says "already exists on origin" "  and says so"
reset_fixture
rm -rf "$T/origins/someone-else"
mkdir -p "$T/origins/someone-else"
git clone -q --bare "$bare" "$T/origins/someone-else/$(basename "$REPO").git"
gitc remote set-url origin "$T/origins/someone-else/$(basename "$REPO").git"
gitc fetch -q origin
rel v0.1.0
refused_early "an origin that is not $REPO (though it is a working repository) is refused"
says "origin is someone-else/$(basename "$REPO"), not $REPO" "  naming both"
reset_fixture
rel v0.2.0
refused_early "a version that is not the newest CHANGELOG version is refused"
says "newest version in CHANGELOG.md is '0.1.0', not 0.2.0" "  naming both"
reset_fixture
printf '# Changelog\n\n## [Unreleased]\n\n- something not released yet\n\n## [0.1.0] - 2026-11-01\n\n- first\n' >"$fix/CHANGELOG.md"
gitc commit -q -am "unreleased entries"
gitc push -q origin main
rel v0.1.0
refused_early "entries left under Unreleased are refused"
says "under Unreleased" "  and says so"
reset_fixture
printf '# Changelog\n\n## [0.1.0] - 2026-11-01\n\n' >"$fix/CHANGELOG.md"
gitc commit -q -am "empty section"
gitc push -q origin main
rel v0.1.0
refused_early "an empty CHANGELOG section is refused"
if [ "$HAS_VERSION_FILE" = 1 ]; then
  reset_fixture
  sed -i.bak 's/Version = ".*"/Version = "0.9.9"/' "$fix/internal/version/version.go" && rm -f "$fix/internal/version/version.go.bak"
  gitc commit -q -am "version mismatch"
  gitc push -q origin main
  rel v0.1.0
  refused_early "a version.go that disagrees with the tag is refused"
  says "internal/version/version.go says '0.9.9'" "  naming both"
fi

reset_fixture
write_changelog " (unreleased)"
gitc commit -q -am "heading not dated"
gitc push -q origin main
rel v0.1.0 --publish --yes
refused_early "--publish with a CHANGELOG heading that still says unreleased is refused"
says "still says" "  and says so"
check "  the fake gh was never called" test "$(calls "$FAKE_GH_LOG")" -eq 0
rel v0.1.0
rc_is 0 "the same heading is only a warning in a dry run"
says "WARN the CHANGELOG.md heading still says" "  and the warning is printed"

reset_fixture
FAKE_SSH_UNREACHABLE=1 rel v0.1.0
rc_is 1 "an unreachable build host is refused"
says "cannot reach $HOSTNAME_FOR_TESTS over ssh" "  with a clear message"
printf '%s\n' ' 07:54:19 up 2 days, load average: 6.00, 6.50, 7.00' 20 'tmpfs           6042124 6000000    100000      99% /tmp' >"$FAKE_SSH_STATUS"
: >"$FAKE_SSH_LOG"
rel v0.1.0
rc_is 1 "a build host with less than 1 GB free is refused"
says "less than 1 GB free" "  with a clear message"
check "  and no build was started (only the status call)" test "$(calls "$FAKE_SSH_LOG")" -eq 1

# ---------------------------------------------------------------------------------------------------
section "the dry run: build, verify, plan, and nothing published"
reset_fixture
mkdir -p "$fix/dist"
echo "old build" >"$fix/dist/old-marker"
printf '%s\n' ' 07:54:19 up 2 days, load average: 99.00, 90.00, 80.00' 20 'tmpfs           6042124 1726552   4315572      29% /tmp' >"$FAKE_SSH_STATUS"
rel v0.1.0
rc_is 0 "a clean main with a dated heading: the dry run exits 0"
says "WARN $HOSTNAME_FOR_TESTS is busy (load 99.00 on 20 cores)" "  a busy host is a warning, not a refusal"
status_ok
reset_fixture
rel v0.1.0
rc_is 0 "the dry run exits 0"
says "origin is $REPO" "  guard: origin"
says "working tree is clean" "  guard: clean tree"
says "tag v0.1.0 does not exist yet" "  guard: tag"
says "4 archives, every sha256 matches checksums.txt" "  verified the archives against checksums.txt"
says "binary: --version prints 0.1.0" "  ran this machine's binary and checked --version"
says "upload plan for $REPO v0.1.0" "  printed the upload plan"
says "gh release create v0.1.0 --repo $REPO --target" "  with the exact gh command"
says "first release notes line" "  and the release notes from CHANGELOG.md"
says "DRY RUN: nothing was uploaded and no tag was created" "  and says it was a dry run"
for p in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  says "dist/${TOOL}_0.1.0_${p}.tar.gz" "  the plan lists the $p archive"
done
says "dist/checksums.txt" "  the plan lists checksums.txt"
says "install.sh" "  the plan lists install.sh"
check "  exactly two ssh connections: the status check and the build" test "$(calls "$FAKE_SSH_LOG")" -eq 2
check "  the build ssh got a tar stream of the committed tree (no .git)" is_tar_without_git
check "  the real gh was not involved: the fake gh was never called" test "$(calls "$FAKE_GH_LOG")" -eq 0
check "  dist/ is the new build (the old marker is gone)" test ! -e "$fix/dist/old-marker" -a -f "$fix/dist/checksums.txt"
check "  the working tree is still clean afterwards (dist/ is ignored)" test -z "$(git -C "$fix" status --porcelain)"
check "  no tag was created locally or on origin" test -z "$(git -C "$fix" tag -l)$(git -C "$bare" tag -l)"
check "  no temporary files left in TMPDIR" test -z "$(ls -A "$TMPDIR")"

# ---------------------------------------------------------------------------------------------------
section "what comes back from the build host is verified"
reset_fixture
mkdir -p "$fix/dist"
echo "old build" >"$fix/dist/old-marker"
FAKE_SSH_BUILD=fail rel v0.1.0
rc_is 1 "a failing remote build is refused"
says "the build on $HOSTNAME_FOR_TESTS failed" "  with a clear message"
check "  the previous dist/ is left alone" test -f "$fix/dist/old-marker"

mk_dist "$FAKE_DIST" 0.1.0
printf 'tampered' >>"$FAKE_DIST/${TOOL}_0.1.0_linux_amd64.tar.gz"
rel v0.1.0
rc_is 1 "an archive that does not match checksums.txt is refused"
says "checksum mismatch for ${TOOL}_0.1.0_linux_amd64.tar.gz" "  naming the archive"
check "  the previous dist/ is left alone" test -f "$fix/dist/old-marker"
mk_dist "$FAKE_DIST" 0.1.0
rm "$FAKE_DIST/${TOOL}_0.1.0_darwin_arm64.tar.gz"
rel v0.1.0
rc_is 1 "an archive that is listed but missing is refused"
mk_dist "$FAKE_DIST" 0.1.0
grep -v linux_arm64 "$FAKE_DIST/checksums.txt" >"$FAKE_DIST/c.new" && mv "$FAKE_DIST/c.new" "$FAKE_DIST/checksums.txt"
rel v0.1.0
rc_is 1 "a platform missing from checksums.txt is refused"
mk_dist "$FAKE_DIST" 0.1.0
mv "$FAKE_DIST/${TOOL}_0.1.0_linux_arm64.tar.gz" "$FAKE_DIST/${TOOL}_0.1.0_freebsd_amd64.tar.gz"
sed 's/linux_arm64/freebsd_amd64/' "$FAKE_DIST/checksums.txt" >"$FAKE_DIST/c.new" && mv "$FAKE_DIST/c.new" "$FAKE_DIST/checksums.txt"
rel v0.1.0
rc_is 1 "four archives with one platform swapped for another are refused"
says "has no entry for ${TOOL}_0.1.0_linux_arm64.tar.gz" "  naming the missing platform"
mk_dist "$FAKE_DIST" 0.1.0
cp "$FAKE_DIST/${TOOL}_0.1.0_linux_arm64.tar.gz" "$FAKE_DIST/${TOOL}_0.1.0_freebsd_amd64.tar.gz"
rel v0.1.0
rc_is 1 "an archive that checksums.txt does not list is refused"
mk_dist "$FAKE_DIST" 0.2.0
rel v0.1.0
rc_is 1 "archives of another version are refused"
mk_dist "$FAKE_DIST" 0.1.0 9.9.9
rel v0.1.0
rc_is 1 "a binary that prints another version for --version is refused"
says "expected exactly '0.1.0'" "  naming what it expected"
mk_dist "$FAKE_DIST" 0.1.0
dup=$(head -n 1 "$FAKE_DIST/checksums.txt")
printf '%s\n' "$dup" >>"$FAKE_DIST/checksums.txt"
rel v0.1.0
rc_is 1 "a checksums.txt that lists an archive twice is refused"
mk_dist "$FAKE_DIST" 0.1.0
printf '%s  %s\n' "not-a-checksum" "${TOOL}_0.1.0_linux_arm64.tar.gz" >>"$FAKE_DIST/checksums.txt"
rel v0.1.0
rc_is 1 "a malformed line in checksums.txt is refused"
good_dist

# ---------------------------------------------------------------------------------------------------
section "--publish (only ever reaches the fake gh)"
reset_fixture
rel v0.1.0 --publish
rc_is 1 "--publish without --yes and without a terminal is refused"
says "no terminal to ask on" "  with a clear message"
check "  and gh was asked nothing that creates anything" test -z "$(grep 'release create' "$FAKE_GH_LOG")"
reset_fixture
FAKE_GH_AUTH=0 rel v0.1.0 --publish --yes
rc_is 1 "--publish with gh not logged in is refused"
says "gh is not logged in" "  with a clear message"
reset_fixture
FAKE_GH_EXISTS=1 rel v0.1.0 --publish --yes
rc_is 1 "--publish when the release already exists is refused"
says "a release for v0.1.0 already exists" "  with a clear message"
check "  nothing was created" test -z "$(grep 'release create' "$FAKE_GH_LOG")"
reset_fixture
: >"$FAKE_GH_NOTES"
rel v0.1.0 --publish --yes
rc_is 0 "--publish --yes runs to the end"
check "  gh release create was called once" test "$(grep -c 'release create' "$FAKE_GH_LOG")" -eq 1
create_line=$(grep 'release create' "$FAKE_GH_LOG")
case $create_line in
  *"release create v0.1.0 --repo $REPO --target $base_head --title $TOOL v0.1.0 --notes-file "*) pass "  with the tag, the repository, the built commit and the title" ;;
  *) fail "  with the tag, the repository, the built commit and the title (got: $create_line)" ;;
esac
for p in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  case $create_line in *"dist/${TOOL}_0.1.0_${p}.tar.gz"*) pass "  uploading the $p archive" ;; *) fail "  uploading the $p archive" ;; esac
done
case $create_line in *"dist/checksums.txt install.sh") pass "  and checksums.txt and install.sh, last" ;; *) fail "  and checksums.txt and install.sh, last (got: $create_line)" ;; esac
check "  the notes are the CHANGELOG section of the version" is_true test "$(cat "$FAKE_GH_NOTES")" = "### Added

- first release notes line
- second line"
check "  the fake gh was asked whether the release exists first" grep -q 'release view v0.1.0' "$FAKE_GH_LOG"
check "  exactly two ssh connections even when publishing" test "$(calls "$FAKE_SSH_LOG")" -eq 2
check "  no tag was created by the script itself (gh creates it)" test -z "$(git -C "$fix" tag -l)$(git -C "$bare" tag -l)"

# ---------------------------------------------------------------------------------------------------
printf '\n%d passed, %d failed\n' "$n_pass" "$n_fail"
if [ "$n_fail" -ne 0 ]; then
  printf 'FAILED\n'
  exit 1
fi
printf 'ALL OK\n'
