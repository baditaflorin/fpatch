#!/usr/bin/env bash
# Tests for install.sh, run against a REAL dist/ (the archives `make dist` or scripts/release.sh built),
# in a scratch HOME and a scratch TMPDIR. Nothing outside the scratch directory is touched: the real
# ~/.local/bin and every other file of yours stay as they are.
#
#   scripts/test-install.sh [DIST_DIR]        DIST_DIR defaults to dist/
#   BASH_BIN=/bin/bash /bin/bash scripts/test-install.sh dist     use macOS's bash 3.2 for both
#
# The gh and curl branch is exercised through fake commands on PATH that serve
# the release from DIST_DIR: no network is used and no real release is needed.
# Works with bash 3.2 (no associative arrays, mapfile or ${v,,}). Exit 0 when everything passed.
set -u

TOOL=fpatch
REPO=baditaflorin/fpatch
NEXT_MARK=''

BASH_BIN=$(command -v "${BASH_BIN:-bash}") || { echo "bash not found" >&2; exit 2; }
here=$(cd "$(dirname "$0")/.." && pwd)
installer=$here/install.sh
dist_arg=${1:-$here/dist}
if [ ! -f "$dist_arg/checksums.txt" ]; then
  echo "test-install: no $dist_arg/checksums.txt: build a real dist first (scripts/release.sh vX.Y.Z, or make dist VERSION=vX.Y.Z)" >&2
  exit 2
fi
dist=$(cd "$dist_arg" && pwd)

case $(uname -s) in Darwin) os=darwin ;; Linux) os=linux ;; *) echo "test-install: unsupported OS $(uname -s)" >&2; exit 2 ;; esac
case $(uname -m) in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "test-install: unsupported CPU $(uname -m)" >&2; exit 2 ;; esac
archive=
for f in "$dist/${TOOL}_"*"_${os}_${arch}.tar.gz"; do archive=${f##*/}; break; done
if [ ! -f "$dist/$archive" ]; then echo "test-install: dist has no $TOOL archive for $os/$arch" >&2; exit 2; fi
ver=${archive#"${TOOL}_"}
ver=${ver%"_${os}_${arch}.tar.gz"}

real_uname=$(command -v uname)
base_path=$PATH
T=$(mktemp -d "${TMPDIR:-/tmp}/$TOOL-test.XXXXXX") || exit 2
trap 'chmod -R u+rwx "$T" 2>/dev/null; rm -rf "$T"' EXIT
export HOME=$T/home TMPDIR=$T/tmp
mkdir -p "$HOME" "$TMPDIR" "$T/shims"
prefix=$HOME/.local/bin
bin=$prefix/$TOOL

# ---- tiny test harness ----------------------------------------------------------------------------

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
is_true() { if "$@"; then return 0; else return 1; fi; }
check() { local name=$1; shift; if "$@"; then pass "$name"; else fail "$name"; fi; }

cur_path=$base_path
inst() { out=$(PATH=$cur_path "$BASH_BIN" "$installer" "$@" 2>&1); rc=$?; }
inst_in() { local d=$1; shift; out=$(cd "$d" && PATH=$cur_path "$BASH_BIN" "$installer" "$@" 2>&1); rc=$?; }
snap() { (cd "$T" && find home tmp | sort); }
hash_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{ print $1 }'; else shasum -a 256 "$1" | awk '{ print $1 }'; fi
}
# shellcheck disable=SC2012  # ls -i is the portable way to read an inode number
inode_of() { ls -i "$1" | awk '{ print $1 }'; }
no_leftovers() { [ -z "$(ls -A "$TMPDIR")" ]; }
no_staged() { [ -z "$(find "$1" -name ".$TOOL.new.*" 2>/dev/null)" ]; }

# flip_byte FILE: change one byte in the middle of FILE, in place
flip_byte() {
  local f=$1 size off
  size=$(wc -c <"$f" | tr -d ' ')
  off=$((size / 2))
  cp "$f" "$f.orig"
  printf '\377' | dd of="$f" bs=1 seek="$off" conv=notrunc 2>/dev/null
  if cmp -s "$f" "$f.orig"; then printf '\000' | dd of="$f" bs=1 seek="$off" conv=notrunc 2>/dev/null; fi
  rm -f "$f.orig"
}

# copy_dist NAME: a writable copy of the real dist/ (this platform's archive and checksums.txt)
copy_dist() {
  mkdir -p "$T/$1"
  cp "$dist/$archive" "$dist/checksums.txt" "$T/$1/"
}

# mk_fake_dist DIR VERSION REPORTS: an archive whose binary is a shell script that prints REPORTS
mk_fake_dist() {
  local d=$1 fv=$2 reports=$3 name
  name=${TOOL}_${fv}_${os}_${arch}.tar.gz
  mkdir -p "$d/stage"
  printf '#!/bin/sh\necho %s\n' "$reports" >"$d/stage/$TOOL"
  chmod 755 "$d/stage/$TOOL"
  echo "fake readme" >"$d/stage/README.md"
  (cd "$d/stage" && tar -cf - "$TOOL" README.md | gzip -n >"../$name")
  rm -rf "$d/stage"
  printf '%s  %s\n' "$(hash_of "$d/$name")" "$name" >"$d/checksums.txt"
}

# ---- fake gh, curl and uname -----------------------------------------------------------------------

cat >"$T/shims/gh" <<'SHIM'
#!/bin/sh
# fake gh: serves the release in $FAKE_GH_DIR (the only tag it knows is $FAKE_GH_LATEST) and logs every call
echo "gh $*" >>"$FAKE_GH_LOG"
case "$1 $2" in
  "auth status")
    if [ "${FAKE_GH_AUTH:-1}" = 1 ]; then exit 0; fi
    echo "You are not logged into any GitHub hosts. To log in, run: gh auth login" >&2
    exit 1 ;;
esac
if [ "$1" = api ]; then
  case "$2" in
    repos/*/releases/latest) echo "$FAKE_GH_LATEST"; exit 0 ;;
  esac
  echo "fake gh: unsupported api call: $2" >&2
  exit 1
fi
if [ "$1 $2" = "release download" ]; then
  tag=$3
  shift 3
  dir=.
  pats=
  while [ $# -gt 0 ]; do
    case "$1" in
      --repo) shift 2 ;;
      --pattern) pats="$pats $2"; shift 2 ;;
      --dir) dir=$2; shift 2 ;;
      *) shift ;;
    esac
  done
  if [ "$tag" != "$FAKE_GH_LATEST" ]; then echo "release not found" >&2; exit 1; fi
  mkdir -p "$dir"
  n=0
  for p in $pats; do
    if [ -f "$FAKE_GH_DIR/$p" ]; then cp "$FAKE_GH_DIR/$p" "$dir/$p"; n=$((n + 1)); fi
  done
  [ "$n" -gt 0 ] || { echo "no assets match the file pattern" >&2; exit 1; }
  exit 0
fi
echo "fake gh: unsupported: $*" >&2
exit 1
SHIM

cat >"$T/shims/curl" <<'SHIM'
#!/bin/sh
# fake curl: serves GitHub release URLs from $FAKE_GH_DIR (tag $FAKE_GH_LATEST) and logs every call
echo "curl $*" >>"$FAKE_GH_LOG"
out=
fmt=
url=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -w) fmt=$2; shift 2 ;;
    --proto|--retry) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
case "$url" in
  https://github.com/*/releases/latest)
    repo=${url#https://github.com/}
    repo=${repo%/releases/latest}
    if [ "$fmt" = '%{url_effective}' ]; then printf '%s' "https://github.com/$repo/releases/tag/$FAKE_GH_LATEST"; fi
    exit 0 ;;
  https://github.com/*/releases/download/*/*)
    rest=${url#https://github.com/*/releases/download/}
    tag=${rest%%/*}
    file=${rest#*/}
    if [ "$tag" = "$FAKE_GH_LATEST" ] && [ -f "$FAKE_GH_DIR/$file" ]; then cp "$FAKE_GH_DIR/$file" "$out"; exit 0; fi
    echo "curl: (22) The requested URL returned error: 404" >&2
    exit 22 ;;
esac
echo "fake curl: unsupported url: $url" >&2
exit 1
SHIM

cat >"$T/shims/uname" <<'SHIM'
#!/bin/sh
case "$1" in
  -s) echo "${FAKE_UNAME_S:-$(@REAL_UNAME@ -s)}" ;;
  -m) echo "${FAKE_UNAME_M:-$(@REAL_UNAME@ -m)}" ;;
  *) exec @REAL_UNAME@ "$@" ;;
esac
SHIM
sed "s#@REAL_UNAME@#$real_uname#g" "$T/shims/uname" >"$T/shims/uname.new" && mv "$T/shims/uname.new" "$T/shims/uname"
chmod 755 "$T"/shims/*

export FAKE_GH_DIR=$dist FAKE_GH_LOG=$T/gh.log FAKE_GH_LATEST=v$ver FAKE_GH_AUTH=1
: >"$FAKE_GH_LOG"
shims_path=$T/shims:$base_path

# mkmin DIR NAME...: a PATH directory holding only the named tools (as symlinks)
mkmin() {
  local d=$1 t p
  shift
  mkdir -p "$d"
  for t in "$@"; do
    p=$(command -v "$t" 2>/dev/null) || continue
    case $p in /*) ln -s "$p" "$d/$t" ;; esac
  done
}
# mkcore DIR NAME...: mkmin with the tools the installer needs, plus NAME...
mkcore() {
  local d=$1
  shift
  mkmin "$d" uname awk tar gzip mktemp install mv rm cmp cp tr sort grep chmod mkdir cat sed head wc ls find od dd id dirname basename readlink sleep "$@"
}

printf 'bash:     %s\nplatform: %s/%s\ndist:     %s (%s %s)\nscratch:  %s\n' \
  "$("$BASH_BIN" --version | head -n 1)" "$os" "$arch" "$dist" "$TOOL" "$ver" "$T"

# ---------------------------------------------------------------------------------------------------
section "arguments"
inst --help
rc_is 0 "--help exits 0"
says "--from-dir" "--help lists the flags"
says "--uninstall" "--help lists --uninstall"
inst --bogus
rc_is 2 "an unknown flag is a usage error (exit 2)"
inst --status --uninstall
rc_is 2 "--status and --uninstall together are refused (exit 2)"
inst --version banana
rc_is 2 "a malformed --version is refused (exit 2)"
inst --repo nonsense
rc_is 2 "a malformed --repo is refused (exit 2)"
inst --prefix
rc_is 2 "--prefix without a value is refused (exit 2)"
out=$(PATH=$cur_path "$BASH_BIN" -s -- --from-dir "$dist" --status <"$installer" 2>&1)
rc=$?
rc_is 1 "run through 'bash -s' (piped, no \$0): works, and --status exits 1 when not installed"
inst --from-dir "$T/no-such-dir" --status
rc_is 1 "--from-dir with a missing directory is an error"
mkdir "$T/empty-dir"
inst --from-dir "$T/empty-dir"
rc_is 1 "--from-dir without checksums.txt is an error"
says "checksums.txt" "  and says what is missing"

# ---------------------------------------------------------------------------------------------------
section "status, dry run and a fresh install"
inst --from-dir "$dist" --status
rc_is 1 "--status before installing exits 1"
says "not installed" "  and says so"

before=$(snap)
inst --from-dir "$dist" --dry-run
rc_is 0 "--dry-run exits 0"
says "would install $ver at $bin" "  and says what it would do"
check "--dry-run wrote nothing at all (HOME and TMPDIR unchanged)" is_true test "$before" = "$(snap)"

inst --from-dir "$dist"
rc_is 0 "fresh install exits 0"
says "checksum ok" "  verified the checksum"
says "installed $ver at $bin" "  and says what it installed"
check "the binary is in the default prefix and executable" test -x "$bin"
got=$("$bin" --version 2>&1)
check "the installed binary prints exactly $ver for --version" is_true test "$got" = "$ver"
check "no temporary files left in TMPDIR" no_leftovers
check "no staged file left in the prefix" no_staged "$prefix"
expected=$(printf 'home\nhome/.local\nhome/.local/bin\nhome/.local/bin/%s\ntmp\n' "$TOOL")
check "nothing was written but the prefix and the binary" is_true test "$(snap)" = "$expected"
if [ -n "$NEXT_MARK" ]; then
  says "$NEXT_MARK" "the next step is printed"
  check "settings.json was not created or touched" test ! -e "$HOME/.claude"
fi

inst --from-dir "$dist" --status
rc_is 0 "--status after installing exits 0"
says "installed $ver" "  reports the installed version"
says "NOT on PATH" "  says the prefix is not on PATH"
says "up to date" "  and that it is up to date"
cur_path=$prefix:$base_path
inst --from-dir "$dist" --status
says "$prefix is on PATH" "--status sees a prefix that is on PATH"
inst --dry-run --from-dir "$dist"
says "would replace $ver with $ver" "--dry-run over an installed copy says it would replace it"
cur_path=$base_path

# ---------------------------------------------------------------------------------------------------
section "idempotence"
sum1=$(hash_of "$bin")
ino1=$(inode_of "$bin")
before=$(snap)
inst --from-dir "$dist"
rc_is 0 "re-running the install exits 0"
says "already installed" "  and says nothing changed"
check "the binary was not rewritten (same inode, same bytes)" is_true test "$(inode_of "$bin")" = "$ino1" -a "$(hash_of "$bin")" = "$sum1"
check "the file tree is unchanged" is_true test "$before" = "$(snap)"
inst --from-dir "$dist" --version "v$ver"
rc_is 0 "--version v$ver (the same version) exits 0"
inst --from-dir "$dist" --version "$ver"
rc_is 0 "--version $ver (no leading v) exits 0"
inst --from-dir "$dist" --version v9.9.9
rc_is 1 "a version that is not in the directory is refused"
says "9.9.9" "  and names it"
check "  the installed binary is untouched" is_true test "$(hash_of "$bin")" = "$sum1" -a "$(inode_of "$bin")" = "$ino1"

# ---------------------------------------------------------------------------------------------------
section "--prefix"
custom=$T/custom/bin/deeper
inst --from-dir "$dist" --prefix "$custom"
rc_is 0 "--prefix to a new nested directory exits 0"
check "  created it and installed there" test -x "$custom/$TOOL"
says "created $custom" "  and says it created the directory"
check "  the default prefix copy is untouched" is_true test "$(inode_of "$bin")" = "$ino1"
inst_in "$T" --from-dir "$dist" --prefix rel/bin/
rc_is 0 "a relative --prefix with a trailing slash exits 0"
check "  it is relative to the current directory" test -x "$T/rel/bin/$TOOL"
inst --from-dir "$dist" "--prefix=$T/eq/bin"
rc_is 0 "--prefix=DIR exits 0"
check "  and installs there" test -x "$T/eq/bin/$TOOL"
tilde='~'
inst --from-dir "$dist" --prefix "$tilde/viahome"
rc_is 0 "--prefix ~/DIR (a literal tilde) exits 0"
check "  it means \$HOME/DIR" test -x "$HOME/viahome/$TOOL"
rm -rf "$HOME/viahome"
check "no temporary files left in TMPDIR" no_leftovers

# ---------------------------------------------------------------------------------------------------
section "platform and asset problems"
cur_path=$shims_path
before=$(snap)
FAKE_UNAME_M=riscv64 inst --from-dir "$dist" --prefix "$T/plat1"
rc_is 1 "an unsupported architecture is refused"
says "unsupported architecture 'riscv64'" "  with a clear message"
FAKE_UNAME_S=FreeBSD inst --from-dir "$dist" --prefix "$T/plat2"
rc_is 1 "an unsupported operating system is refused"
says "unsupported operating system 'FreeBSD'" "  with a clear message"
check "  nothing was created" test ! -e "$T/plat1" -a ! -e "$T/plat2"
copy_dist dist-noarm
grep -v "_linux_arm64.tar.gz" "$dist/checksums.txt" >"$T/dist-noarm/checksums.txt"
rm -f "$T/dist-noarm/${TOOL}_"*"_linux_arm64.tar.gz"
FAKE_UNAME_S=Linux FAKE_UNAME_M=aarch64 inst --from-dir "$T/dist-noarm" --prefix "$T/plat3"
rc_is 1 "a release without a build for this platform is refused"
says "no build for this platform" "  with a clear message"
says "linux/arm64" "  naming the platform"
FAKE_UNAME_S=Linux FAKE_UNAME_M=aarch64 inst --from-dir "$T/dist-noarm" --status
says "linux/arm64" "--status picks the asset from uname -s/-m too (aarch64 means arm64)"
cur_path=$base_path
copy_dist dist-nofile
rm -f "$T/dist-nofile/$archive"
inst --from-dir "$T/dist-nofile" --prefix "$T/plat4"
rc_is 1 "an archive that is listed but missing is refused"
says "has no $archive" "  and names it"
copy_dist dist-nosum
grep -v "$archive" "$dist/checksums.txt" >"$T/dist-nosum/checksums.txt"
inst --from-dir "$T/dist-nosum" --version "v$ver" --prefix "$T/plat5"
rc_is 1 "an archive with no line in checksums.txt is refused"
says "no line for $archive" "  naming the archive"
check "  nothing was installed" test ! -e "$T/plat5"
check "no temporary files left in TMPDIR" no_leftovers

# ---------------------------------------------------------------------------------------------------
section "tampering is refused and the previous binary survives"
copy_dist dist-tamper
flip_byte "$T/dist-tamper/$archive"
inst --from-dir "$T/dist-tamper"
rc_is 1 "a tampered archive is refused"
says "checksum mismatch" "  with a clear message"
says "nothing was installed" "  and says nothing changed"
check "  the previous binary is intact (same inode, same bytes)" is_true test "$(hash_of "$bin")" = "$sum1" -a "$(inode_of "$bin")" = "$ino1"
inst --from-dir "$T/dist-tamper" --prefix "$T/tamper-fresh"
rc_is 1 "a tampered archive is refused on a fresh prefix too"
check "  and the prefix was not even created" test ! -e "$T/tamper-fresh"
check "  no temporary files left in TMPDIR" no_leftovers
inst --from-dir "$T/dist-tamper" --dry-run
rc_is 1 "--dry-run also refuses a tampered archive"

copy_dist dist-badsum
awk -v a="$archive" '$2 == a { c = substr($1, 1, 1); $1 = (c == "0" ? "1" : "0") substr($1, 2) } { print $1 "  " $2 }' "$dist/checksums.txt" >"$T/dist-badsum/checksums.txt"
inst --from-dir "$T/dist-badsum"
rc_is 1 "a tampered checksums.txt (one digit changed) is refused"
says "checksum mismatch" "  with a clear message"
check "  the previous binary is intact" is_true test "$(hash_of "$bin")" = "$sum1" -a "$(inode_of "$bin")" = "$ino1"
copy_dist dist-dupsum
{ cat "$dist/checksums.txt"; printf '%s  %s\n' "$(printf '%064d' 0)" "$archive"; } >"$T/dist-dupsum/checksums.txt"
inst --from-dir "$T/dist-dupsum"
rc_is 1 "a checksums.txt that lists the archive twice is refused"
says "more than once" "  with a clear message"
copy_dist dist-junksum
awk -v a="$archive" '$2 == a { $1 = "not-a-checksum" } { print $1 "  " $2 }' "$dist/checksums.txt" >"$T/dist-junksum/checksums.txt"
inst --from-dir "$T/dist-junksum"
rc_is 1 "a malformed checksum is refused"
copy_dist dist-crlf
sed 's/$/\r/' "$dist/checksums.txt" >"$T/dist-crlf/checksums.txt"
inst --from-dir "$T/dist-crlf" --version "v$ver"
rc_is 1 "a checksums.txt with CRLF line ends is refused rather than guessed at"
: >"$T/dist-badsum/checksums.txt"
inst --from-dir "$T/dist-badsum" --version "v$ver"
rc_is 1 "an empty checksums.txt is refused"
says "missing or empty" "  with a clear message"
check "the previous binary is still intact" is_true test "$(hash_of "$bin")" = "$sum1" -a "$(inode_of "$bin")" = "$ino1"

# ---------------------------------------------------------------------------------------------------
section "odd archives (checksums are valid; the contents are not)"
mk_fake_dist "$T/fake-good" 0.0.7 0.0.7
inst --from-dir "$T/fake-good" --prefix "$T/fake-prefix"
rc_is 0 "a well-formed fake archive installs"
got=$("$T/fake-prefix/$TOOL" --version 2>&1)
check "  and runs" is_true test "$got" = "0.0.7"
mk_fake_dist "$T/fake-lie" 0.0.7 9.9.9
inst --from-dir "$T/fake-lie" --prefix "$T/fake-prefix2"
rc_is 1 "an archive whose binary reports another version is refused"
says "reports '9.9.9', not '0.0.7'" "  naming both"
check "  nothing was installed" test ! -e "$T/fake-prefix2"
mkdir -p "$T/fake-nobin"
echo hi >"$T/fake-nobin/README.md"
(cd "$T/fake-nobin" && tar -cf - README.md | gzip -n >"${TOOL}_0.0.7_${os}_${arch}.tar.gz" && rm README.md)
printf '%s  %s\n' "$(hash_of "$T/fake-nobin/${TOOL}_0.0.7_${os}_${arch}.tar.gz")" "${TOOL}_0.0.7_${os}_${arch}.tar.gz" >"$T/fake-nobin/checksums.txt"
inst --from-dir "$T/fake-nobin" --prefix "$T/fake-prefix3"
rc_is 1 "an archive without the binary is refused"
says "does not contain a file named $TOOL" "  with a clear message"
mkdir -p "$T/fake-link"
(cd "$T/fake-link" && ln -s /bin/sh "$TOOL" && tar -cf - "$TOOL" | gzip -n >"${TOOL}_0.0.7_${os}_${arch}.tar.gz" && rm "$TOOL")
printf '%s  %s\n' "$(hash_of "$T/fake-link/${TOOL}_0.0.7_${os}_${arch}.tar.gz")" "${TOOL}_0.0.7_${os}_${arch}.tar.gz" >"$T/fake-link/checksums.txt"
inst --from-dir "$T/fake-link" --prefix "$T/fake-prefix4"
rc_is 1 "an archive whose binary is a symlink is refused"
mkdir -p "$T/fake-junk"
echo "this is not a tar.gz" >"$T/fake-junk/${TOOL}_0.0.7_${os}_${arch}.tar.gz"
printf '%s  %s\n' "$(hash_of "$T/fake-junk/${TOOL}_0.0.7_${os}_${arch}.tar.gz")" "${TOOL}_0.0.7_${os}_${arch}.tar.gz" >"$T/fake-junk/checksums.txt"
inst --from-dir "$T/fake-junk" --prefix "$T/fake-prefix5"
rc_is 1 "a file that is not a tar.gz is refused"
check "  nothing was installed by any of them" test ! -e "$T/fake-prefix3" -a ! -e "$T/fake-prefix4" -a ! -e "$T/fake-prefix5"
check "no temporary files left in TMPDIR" no_leftovers

# ---------------------------------------------------------------------------------------------------
section "the prefix is not usable"
mkdir -p "$T/lp"
ln -s /bin/sh "$T/lp/$TOOL"
inst --from-dir "$dist" --prefix "$T/lp"
rc_is 1 "install over a symlink is refused"
says "symlink" "  with a clear message"
check "  the symlink is untouched" test -L "$T/lp/$TOOL"
inst --uninstall --prefix "$T/lp"
rc_is 1 "uninstall of a symlink is refused"
check "  the symlink is untouched" test -L "$T/lp/$TOOL"
mkdir -p "$T/dp/$TOOL"
inst --from-dir "$dist" --prefix "$T/dp"
rc_is 1 "install where the target is a directory is refused"
says "is a directory" "  with a clear message"
if [ "$(id -u)" -eq 0 ]; then
  printf 'skip read-only prefix tests (running as root: file modes do not apply)\n'
else
  mkdir -p "$T/ro"
  chmod 555 "$T/ro"
  inst --from-dir "$dist" --prefix "$T/ro/bin"
  rc_is 1 "a prefix that cannot be created is refused"
  says "cannot create" "  with a clear message"
  mkdir -p "$T/ro2"
  chmod 555 "$T/ro2"
  inst --from-dir "$dist" --prefix "$T/ro2"
  rc_is 1 "an existing prefix that cannot be written is refused"
  says "cannot write" "  with a clear message"
  chmod 755 "$T/ro" "$T/ro2"
  check "  no staged file left behind" no_staged "$T/ro2"
fi
check "no temporary files left in TMPDIR" no_leftovers

# ---------------------------------------------------------------------------------------------------
section "tools missing from PATH"
mkcore "$T/min-both" sha256sum shasum
mkcore "$T/min-sha256sum" sha256sum
mkcore "$T/min-shasum" shasum
mkcore "$T/min-none"
mkcore "$T/min-noawk" sha256sum shasum
rm -f "$T/min-noawk/awk"
cur_path=$T/min-none
inst --from-dir "$dist" --prefix "$T/nh"
rc_is 1 "without sha256sum or shasum the install is refused"
says "sha256sum or shasum" "  with a clear message"
check "  nothing was installed" test ! -e "$T/nh"
cur_path=$T/min-noawk
inst --from-dir "$dist" --prefix "$T/na"
rc_is 1 "without awk the install is refused"
says "missing required tools: awk" "  naming the tool"
if command -v sha256sum >/dev/null 2>&1; then
  cur_path=$T/min-sha256sum
  inst --from-dir "$dist" --prefix "$T/h1"
  rc_is 0 "with only sha256sum the install works"
  check "  and installs" test -x "$T/h1/$TOOL"
fi
if command -v shasum >/dev/null 2>&1; then
  cur_path=$T/min-shasum
  inst --from-dir "$dist" --prefix "$T/h2"
  rc_is 0 "with only shasum the install works"
  check "  and installs" test -x "$T/h2/$TOOL"
fi
cur_path=$base_path

# ---------------------------------------------------------------------------------------------------
section "the gh branch (through a fake gh that serves dist/; no network)"
cur_path=$shims_path
: >"$FAKE_GH_LOG"
inst --prefix "$T/gh1"
rc_is 0 "install with no --from-dir uses gh and exits 0"
says "gh release download" "  and says it used gh"
check "  installed the binary" test -x "$T/gh1/$TOOL"
got=$("$T/gh1/$TOOL" --version 2>&1)
check "  and it prints $ver" is_true test "$got" = "$ver"
check "  asked for the latest release of $REPO" grep -q "api repos/$REPO/releases/latest" "$FAKE_GH_LOG"
check "  downloaded the archive and checksums.txt of one release, from $REPO" \
  grep -q -- "release download v$ver --repo $REPO --pattern $archive --pattern checksums.txt --dir" "$FAKE_GH_LOG"
check "no temporary files left in TMPDIR" no_leftovers

: >"$FAKE_GH_LOG"
inst --prefix "$T/gh2" --version "v$ver"
rc_is 0 "--version vX.Y.Z through gh exits 0"
check "  does not look up the latest release" is_true test -z "$(grep 'releases/latest' "$FAKE_GH_LOG")"
: >"$FAKE_GH_LOG"
inst --prefix "$T/gh3" --repo someone/else
rc_is 0 "--repo is passed on to gh"
check "  the log shows --repo someone/else" grep -q -- "--repo someone/else" "$FAKE_GH_LOG"

: >"$FAKE_GH_LOG"
inst --prefix "$T/gh4" --dry-run
rc_is 0 "--dry-run through gh exits 0"
says "would install $ver" "  and says what it would do"
check "  downloaded nothing and created nothing" is_true test -z "$(grep 'release download' "$FAKE_GH_LOG")" -a ! -e "$T/gh4"

FAKE_GH_DIR=$T/dist-tamper inst --prefix "$T/gh5"
rc_is 1 "a tampered archive through gh is refused"
says "checksum mismatch" "  with a clear message"
check "  nothing was installed" test ! -e "$T/gh5"
inst --prefix "$T/gh6" --version v9.9.9
rc_is 1 "a release gh does not have is refused"
says "gh release download v9.9.9 failed" "  with a clear message"

inst --prefix "$T/gh1" --status
rc_is 0 "--status through gh: up to date exits 0"
says "latest release: v$ver" "  reports the latest release"
says "up to date" "  and says it is up to date"
FAKE_GH_LATEST=v99.0.0 inst --prefix "$T/gh1" --status
rc_is 1 "--status with a newer release available exits 1"
says "OUTDATED" "  and says so"
mk_fake_dist "$T/fake-new" 99.0.0 99.0.0
inst --from-dir "$T/fake-new" --prefix "$T/gh9"
inst --prefix "$T/gh9" --status
rc_is 0 "--status when the installed binary is newer than the latest release exits 0"
says "newer than the latest release" "  and says so"

FAKE_GH_AUTH=0 inst --prefix "$T/gh7"
rc_is 0 "gh not logged in: the public repository still installs, through curl"
says "falling back to curl" "  and says so"
check "  used curl on the release URL" grep -q "curl .*https://github.com/$REPO/releases/download/v$ver/$archive" "$FAKE_GH_LOG"
check "  installed the binary" test -x "$T/gh7/$TOOL"
FAKE_GH_AUTH=0 inst --prefix "$T/gh1" --status
rc_is 0 "--status with the latest release unreachable still exits 0 when installed"
says "latest release: v$ver" "  (it found the release through curl)"

section "the curl branch (public repository, no gh at all; through a fake curl)"
cur_path=$T/min-both
inst --prefix "$T/gh8"
rc_is 1 "neither gh nor curl: refused"
says "curl is not installed either" "  with a clear message"
check "  nothing was installed" test ! -e "$T/gh8"
mkcore "$T/min-curl" sha256sum shasum
cp "$T/shims/curl" "$T/min-curl/curl"
cur_path=$T/min-curl
: >"$FAKE_GH_LOG"
inst --prefix "$T/curl1"
rc_is 0 "no gh installed: install falls back to curl and exits 0"
says "gh (the GitHub CLI) is not installed" "  and says why"
says "falling back to curl" "  and says it did"
got=$("$T/curl1/$TOOL" --version 2>&1)
check "  the installed binary prints $ver" is_true test "$got" = "$ver"
check "  asked GitHub for the latest release" grep -q "curl .*https://github.com/$REPO/releases/latest" "$FAKE_GH_LOG"
check "  fetched the archive and checksums.txt of one release" \
  is_true test "$(grep -c "releases/download/v$ver/" "$FAKE_GH_LOG")" -eq 2
FAKE_GH_DIR=$T/dist-tamper inst --prefix "$T/curl2"
rc_is 1 "a tampered archive through curl is refused"
says "checksum mismatch" "  with a clear message"
check "  nothing was installed" test ! -e "$T/curl2"
inst --prefix "$T/curl3" --version v9.9.9
rc_is 1 "a release curl cannot download is refused"
says "download failed" "  with a clear message"
inst --prefix "$T/curl1" --status
rc_is 0 "--status through curl exits 0"
says "up to date" "  and says it is up to date"
cur_path=$base_path
check "no temporary files left in TMPDIR" no_leftovers

# ---------------------------------------------------------------------------------------------------
section "uninstall removes only its own binary"
mkdir -p "$HOME/.config/$TOOL"
echo cfg >"$HOME/.config/$TOOL/config.json"
echo other >"$prefix/other-tool"
chmod 755 "$prefix/other-tool"
echo sibling >"$prefix/$TOOL.keep"
inst --uninstall --dry-run
rc_is 0 "--uninstall --dry-run exits 0"
says "would remove $bin" "  and says what it would remove"
check "  the binary is still there" test -x "$bin"
inst --uninstall
rc_is 0 "--uninstall exits 0"
says "removed $bin" "  and says what it removed"
check "  the binary is gone" test ! -e "$bin"
check "  another tool in the same prefix is untouched" is_true test "$(cat "$prefix/other-tool")" = other
check "  a similarly named file is untouched" is_true test "$(cat "$prefix/$TOOL.keep")" = sibling
check "  the configuration is untouched" is_true test "$(cat "$HOME/.config/$TOOL/config.json")" = cfg
check "  the prefix directory is kept" test -d "$prefix"
inst --uninstall
rc_is 0 "uninstalling twice exits 0"
says "not installed" "  and says there is nothing to remove"
inst --from-dir "$dist" --status
rc_is 1 "--status after uninstalling exits 1"
check "no temporary files left in TMPDIR" no_leftovers
inst --from-dir "$dist"
rc_is 0 "installing again after an uninstall works"
check "  and the binary is back" test -x "$bin"

# ---------------------------------------------------------------------------------------------------
printf '\n%d passed, %d failed\n' "$n_pass" "$n_fail"
if [ "$n_fail" -ne 0 ]; then
  printf 'FAILED\n'
  exit 1
fi
printf 'ALL OK\n'
