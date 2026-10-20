#!/usr/bin/env bash
# Install fpatch from a GitHub release: download the archive and the release's checksums.txt, verify the
# sha256, and install one binary into a directory you own. Idempotent, no sudo, and nothing is written
# outside the prefix except a temporary directory. It does not touch shell profiles, settings files or
# any other tool's files.
#
#   ./install.sh                         install the latest release into ~/.local/bin
#   ./install.sh --version vX.Y.Z        install that release
#   ./install.sh --prefix DIR            install into DIR instead of ~/.local/bin
#   ./install.sh --repo OWNER/REPO       fetch from another repository (default baditaflorin/fpatch)
#   ./install.sh --from-dir DIR          offline: install from a `make dist` output directory
#   ./install.sh --status                installed version, is the prefix on PATH, latest release
#                                        (exit 1 when not installed or outdated)
#   ./install.sh --uninstall             remove the binary from the prefix, nothing else
#   ./install.sh --dry-run               say what would happen, write nothing (with any of the above)
#
# Downloads use `gh release download`, and when gh is missing or not logged in it
# falls back to curl on the public release URL and says so.
# The only trust anchor is the checksums.txt of the same release; docs/RELEASING.md says what that does
# and does not protect against. Needs bash (3.2 is enough), tar, gzip, awk, and sha256sum or shasum.
set -euo pipefail

TOOL=fpatch
DEFAULT_REPO=baditaflorin/fpatch

tmp=
staged=

cleanup() {
  [ -z "$staged" ] || rm -f "$staged"
  [ -z "$tmp" ] || rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 129' HUP
trap 'exit 143' TERM

say() { printf '%s\n' "$*"; }
note() { printf '%s\n' "$*" >&2; }
die() { printf '%s install: %s\n' "$TOOL" "$*" >&2; exit 1; }
usage_error() { printf '%s install: %s (see --help)\n' "$TOOL" "$*" >&2; exit 2; }

usage() {
  cat <<EOF
Install $TOOL from a GitHub release (checksum-verified, no sudo, one binary).

  install.sh                         install the latest release into ~/.local/bin
  install.sh --version vX.Y.Z        install that release
  install.sh --prefix DIR            install into DIR instead of ~/.local/bin
  install.sh --repo OWNER/REPO       fetch from another repository (default $DEFAULT_REPO)
  install.sh --from-dir DIR          offline: install from a 'make dist' output directory
  install.sh --status                installed version, is the prefix on PATH, latest release
                                     (exit 1 when not installed or outdated)
  install.sh --uninstall             remove the binary from the prefix, nothing else
  install.sh --dry-run               say what would happen, write nothing
EOF
}

# ---- arguments ---------------------------------------------------------------------------------

mode=install
version=latest
repo=$DEFAULT_REPO
prefix=
from_dir=
dry_run=0

set_mode() {
  [ "$mode" = install ] || usage_error "--status and --uninstall cannot be combined"
  mode=$1
}

while [ $# -gt 0 ]; do
  case $1 in
    --version)   [ $# -ge 2 ] || usage_error "--version needs a value"; version=$2; shift 2 ;;
    --version=*) version=${1#--version=}; shift ;;
    --prefix)    [ $# -ge 2 ] || usage_error "--prefix needs a directory"; prefix=$2; shift 2 ;;
    --prefix=*)  prefix=${1#--prefix=}; shift ;;
    --repo)      [ $# -ge 2 ] || usage_error "--repo needs OWNER/REPO"; repo=$2; shift 2 ;;
    --repo=*)    repo=${1#--repo=}; shift ;;
    --from-dir)  [ $# -ge 2 ] || usage_error "--from-dir needs a directory"; from_dir=$2; shift 2 ;;
    --from-dir=*) from_dir=${1#--from-dir=}; shift ;;
    --uninstall) set_mode uninstall; shift ;;
    --status)    set_mode status; shift ;;
    --dry-run)   dry_run=1; shift ;;
    -h|--help)   usage; exit 0 ;;
    *)           usage_error "unknown argument: $1" ;;
  esac
done

repo_re='^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'
[[ $repo =~ $repo_re ]] || usage_error "--repo must look like OWNER/REPO, got '$repo'"

# want_tag is empty for "latest", else vX.Y.Z
version_re='^v?[0-9]+\.[0-9]+\.[0-9]+$'
sum_re='^[0-9a-fA-F]{64}$'
want_tag=
if [ "$version" != latest ]; then
  [[ $version =~ $version_re ]] || usage_error "--version must be latest or vX.Y.Z, got '$version'"
  want_tag=v${version#v}
fi

[ -n "${HOME:-}" ] || die "HOME is not set"
if [ -z "$prefix" ]; then
  prefix=$HOME/.local/bin
fi
tilde='~'
case $prefix in
  "$tilde") prefix=$HOME ;;
  "$tilde"/*) prefix=$HOME/${prefix#"$tilde"/} ;;
esac
case $prefix in
  /*) ;;
  *) prefix=$PWD/$prefix ;;
esac
while [ "$prefix" != / ] && [ "${prefix%/}" != "$prefix" ]; do prefix=${prefix%/}; done

if [ -n "$from_dir" ]; then
  [ -d "$from_dir" ] || die "--from-dir $from_dir is not a directory (build one with: make dist VERSION=vX.Y.Z)"
  from_dir=$(cd "$from_dir" && pwd)
fi

dest=$prefix/$TOOL

# ---- helpers -----------------------------------------------------------------------------------

os=
arch=
detect_platform() {
  local s m
  s=$(uname -s) || die "cannot run uname"
  m=$(uname -m) || die "cannot run uname"
  case $s in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) die "unsupported operating system '$s': releases are built for macOS (darwin) and Linux" ;;
  esac
  case $m in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "unsupported architecture '$m': releases are built for amd64 and arm64" ;;
  esac
}

need() {
  local c missing=
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || missing="$missing $c"
  done
  [ -z "$missing" ] || die "missing required tools:$missing"
}

hasher=
detect_hasher() {
  if command -v sha256sum >/dev/null 2>&1; then
    hasher=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    hasher=shasum
  else
    die "need sha256sum or shasum to verify the download"
  fi
}

sha256_of() {
  if [ "$hasher" = sha256sum ]; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

# the first line `BIN --version` prints, or nothing when it cannot be run
bin_version() {
  local out
  out=$("$1" --version 2>/dev/null) || out=
  printf '%s' "${out%%$'\n'*}"
}

# prints lt, eq or gt for dotted versions a and b, or ? when either is not X.Y.Z
ver_cmp() {
  awk -v a="$1" -v b="$2" 'BEGIN {
    if (a !~ /^[0-9]+\.[0-9]+\.[0-9]+$/ || b !~ /^[0-9]+\.[0-9]+\.[0-9]+$/) { print "?"; exit }
    split(a, x, "."); split(b, y, ".")
    for (i = 1; i <= 3; i++) {
      if (x[i] + 0 < y[i] + 0) { print "lt"; exit }
      if (x[i] + 0 > y[i] + 0) { print "gt"; exit }
    }
    print "eq"
  }'
}

on_path() {
  case ":${PATH:-}:" in
    *":$prefix:"*) return 0 ;;
  esac
  return 1
}

path_hint() {
  if ! on_path; then
    say "note: $prefix is not on your PATH. Add it in your shell profile, for example:"
    say "  export PATH=\"$prefix:\$PATH\""
  fi
}

# the command to show in "next steps": the bare name only when the shell would find this very binary
run_name() {
  local found
  found=$(command -v "$TOOL" 2>/dev/null) || found=
  if [ "$found" = "$dest" ]; then printf '%s' "$TOOL"; else printf '%s' "$dest"; fi
}

# ---- where the release comes from --------------------------------------------------------------

source_kind=
source_err=
choose_source() {
  if [ -n "$from_dir" ]; then
    source_kind=dir
    return 0
  fi
  if command -v gh >/dev/null 2>&1; then
    if gh auth status >/dev/null 2>&1; then
      source_kind=gh
      return 0
    fi
    source_err="gh is installed but 'gh auth status' fails (not logged in? run: gh auth login)"
  else
    source_err="gh (the GitHub CLI) is not installed"
  fi
  if command -v curl >/dev/null 2>&1; then
    source_kind=curl
    return 0
  fi
  source_err="$source_err, and curl is not installed either"
  return 1
}

tag=
resolve_err=
resolve_tag() {
  case $source_kind in
    dir)
      local sums=$from_dir/checksums.txt found n
      [ -f "$sums" ] || { resolve_err="$from_dir has no checksums.txt (build one with: make dist VERSION=vX.Y.Z)"; return 1; }
      found=$(awk -v t="$TOOL" -v o="$os" -v a="$arch" '
        { f = $2; sub(/^\*/, "", f)
          pre = t "_"; suf = "_" o "_" a ".tar.gz"
          if (index(f, pre) == 1 && length(f) > length(pre) + length(suf) && substr(f, length(f) - length(suf) + 1) == suf)
            print substr(f, length(pre) + 1, length(f) - length(pre) - length(suf)) }' "$sums" | sort -u)
      n=$(printf '%s' "$found" | awk 'END { print NR }')
      case $n in
        0) resolve_err="checksums.txt in $from_dir lists no $TOOL archive for $os/$arch (this release has no build for this platform)"; return 1 ;;
        1) tag=v$found ;;
        *) resolve_err="$from_dir holds several versions of $TOOL; pass --version vX.Y.Z"; return 1 ;;
      esac
      ;;
    gh)
      tag=$(gh api "repos/$repo/releases/latest" --jq .tag_name 2>/dev/null) \
        || { resolve_err="gh cannot read the latest release of $repo (no release yet, or no access)"; return 1; }
      ;;
    curl)
      local url
      url=$(curl -fsSIL --proto '=https' -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") \
        || { resolve_err="cannot reach https://github.com/$repo/releases/latest"; return 1; }
      case $url in
        */releases/tag/*) tag=${url##*/releases/tag/} ;;
        *) resolve_err="$repo has no published release yet"; return 1 ;;
      esac
      ;;
    *) resolve_err="no way to look up the latest release"; return 1 ;;
  esac
  [[ $tag =~ $version_re ]] || { resolve_err="the latest release of $repo is tagged '$tag', not vX.Y.Z"; return 1; }
  return 0
}

describe_source() {
  case $source_kind in
    dir) printf '%s' "directory $from_dir" ;;
    gh)  printf '%s' "$repo $tag, with gh release download" ;;
    curl) printf '%s' "$repo $tag, with curl on the public release URL (no authenticated gh)" ;;
  esac
}

# copies (dir) or downloads the archive and checksums.txt of $tag into $1
fetch() {
  local into=$1 f
  case $source_kind in
    dir)
      for f in "$archive" checksums.txt; do
        [ -f "$from_dir/$f" ] || die "$from_dir has no $f"
        cp "$from_dir/$f" "$into/$f" || die "cannot read $from_dir/$f"
      done
      ;;
    gh)
      gh release download "$tag" --repo "$repo" --pattern "$archive" --pattern checksums.txt --dir "$into" \
        || die "gh release download $tag failed for $repo (does that release exist, and can your gh account read it?)"
      for f in "$archive" checksums.txt; do
        [ -f "$into/$f" ] || die "release $tag of $repo has no asset named $f"
      done
      ;;
    curl)
      for f in "$archive" checksums.txt; do
        curl -fsSL --proto '=https' --retry 2 -o "$into/$f" "https://github.com/$repo/releases/download/$tag/$f" \
          || die "download failed: https://github.com/$repo/releases/download/$tag/$f"
      done
      ;;
  esac
}

# verify_in DIR: DIR/$archive must match its one line in DIR/checksums.txt. Sets sum.
sum=
verify_in() {
  local dir=$1 listed n got
  [ -s "$dir/checksums.txt" ] || die "checksums.txt is missing or empty; refusing to install an unverified archive"
  [ -f "$dir/$archive" ] || die "$archive is not in $dir"
  listed=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$dir/checksums.txt")
  n=$(printf '%s' "$listed" | awk 'END { print NR }')
  [ "$n" -ne 0 ] || die "checksums.txt has no line for $archive; refusing to install an unverified archive"
  [ "$n" -eq 1 ] || die "checksums.txt lists $archive more than once; refusing to guess"
  [[ $listed =~ $sum_re ]] || die "checksums.txt has a malformed checksum for $archive"
  listed=$(printf '%s' "$listed" | tr 'A-F' 'a-f')
  got=$(sha256_of "$dir/$archive") || die "cannot compute the sha256 of $dir/$archive"
  if [ "$got" != "$listed" ]; then
    die "checksum mismatch for $archive (checksums.txt says $listed, the file is $got); nothing was installed"
  fi
  sum=$got
}

# ---- commands ----------------------------------------------------------------------------------

do_uninstall() {
  if [ -L "$dest" ]; then
    die "$dest is a symlink, so it was not put there by this installer; remove it yourself if you want it gone"
  fi
  if [ ! -e "$dest" ]; then
    say "$TOOL is not installed in $prefix; nothing to remove"
    return 0
  fi
  [ -f "$dest" ] || die "$dest is not a regular file; not removing it"
  if [ "$dry_run" = 1 ]; then
    say "would remove $dest"
    return 0
  fi
  rm -f "$dest" || die "cannot remove $dest"
  say "removed $dest"
}

do_status() {
  local inst='' rc=0 rel firstbin label=latest
  detect_platform
  if [ -L "$dest" ]; then
    say "$TOOL: $dest is a symlink (not managed by this installer)"
    rc=1
  elif [ ! -e "$dest" ]; then
    say "$TOOL: not installed in $prefix"
    rc=1
  else
    inst=$(bin_version "$dest")
    if [ -n "$inst" ]; then
      say "$TOOL: installed $inst ($dest)"
    else
      say "$TOOL: $dest exists but does not run with --version"
      rc=1
    fi
  fi
  if on_path; then
    say "prefix: $prefix is on PATH"
  else
    say "prefix: $prefix is NOT on PATH"
  fi
  firstbin=$(command -v "$TOOL" 2>/dev/null) || firstbin=
  if [ -n "$firstbin" ] && [ "$firstbin" != "$dest" ]; then
    say "note: a different $TOOL comes first on PATH: $firstbin"
  fi
  [ -z "$want_tag" ] || label=requested
  if choose_source && { if [ -n "$want_tag" ]; then tag=$want_tag; true; else resolve_tag; fi; }; then
    say "$label release: $tag ($(describe_source))"
    if [ -n "$inst" ]; then
      rel=$(ver_cmp "$inst" "${tag#v}")
      case $rel in
        eq) say "up to date" ;;
        lt) say "OUTDATED: installed $inst, latest ${tag#v}"; rc=1 ;;
        gt) say "installed $inst is newer than the latest release ${tag#v} (a local build?)" ;;
        *)  say "installed '$inst' cannot be compared with ${tag#v}"; rc=1 ;;
      esac
    fi
  else
    say "$label release: unknown (${resolve_err:-$source_err})"
  fi
  return "$rc"
}

do_install() {
  local archive ver current='' replacing=''
  detect_platform
  need awk tar gzip mktemp install mv rm cmp cp tr sort grep chmod mkdir
  detect_hasher
  choose_source || die "$source_err"
  if [ "$source_kind" = curl ]; then
    note "$TOOL install: $source_err"
    note "$TOOL install: falling back to curl on the public release URL"
  fi
  if [ -n "$want_tag" ]; then tag=$want_tag; else resolve_tag || die "$resolve_err"; fi
  ver=${tag#v}
  archive=${TOOL}_${ver}_${os}_${arch}.tar.gz

  if [ -L "$dest" ]; then
    die "$dest is a symlink (a local build?); remove it first, or pick another --prefix"
  fi
  [ ! -d "$dest" ] || die "$dest is a directory"
  if [ -f "$dest" ]; then
    current=$(bin_version "$dest")
    replacing=${current:-"a binary that does not run"}
  fi

  say "$TOOL $ver for $os/$arch from $(describe_source)"
  if [ "$dry_run" = 1 ]; then
    if [ "$source_kind" = dir ]; then
      verify_in "$from_dir"
      say "checksum ok: $archive"
    fi
    if [ -n "$replacing" ]; then
      say "would replace $replacing with $ver at $dest (dry run, nothing written)"
    else
      say "would install $ver at $dest (dry run, nothing written)"
    fi
    return 0
  fi

  tmp=$(mktemp -d "${TMPDIR:-/tmp}/$TOOL-install.XXXXXX") || die "cannot create a temporary directory"
  mkdir "$tmp/dl" "$tmp/x"
  fetch "$tmp/dl"
  verify_in "$tmp/dl"
  say "checksum ok: $sum  $archive"

  # extract the one member by name; anything else in the archive is ignored
  tar -tzf "$tmp/dl/$archive" >"$tmp/list" 2>/dev/null || die "$archive is not a readable gzip tar archive"
  grep -qx "$TOOL" "$tmp/list" || die "$archive does not contain a file named $TOOL"
  tar -xzf "$tmp/dl/$archive" -C "$tmp/x" "$TOOL" || die "cannot extract $TOOL from $archive"
  if [ -L "$tmp/x/$TOOL" ] || [ ! -f "$tmp/x/$TOOL" ]; then
    die "$TOOL in $archive is not a regular file"
  fi
  chmod 755 "$tmp/x/$TOOL"

  # a mislabelled archive is refused before anything is touched. A binary that cannot run here
  # (for example a noexec temporary directory) is only a warning.
  local fresh
  fresh=$(bin_version "$tmp/x/$TOOL")
  if [ -n "$fresh" ] && [ "$fresh" != "$ver" ]; then
    die "$archive holds a binary that reports '$fresh', not '$ver'; refusing to install it"
  fi
  [ -n "$fresh" ] || note "$TOOL install: could not run the downloaded binary to check its version (noexec temporary directory?); continuing"

  if [ ! -d "$prefix" ]; then
    mkdir -p "$prefix" || die "cannot create $prefix"
    say "created $prefix"
  fi
  if [ -f "$dest" ] && [ -x "$dest" ] && cmp -s "$tmp/x/$TOOL" "$dest"; then
    say "already installed: $dest is $ver, unchanged"
  else
    staged=$prefix/.$TOOL.new.$$
    install -m 755 "$tmp/x/$TOOL" "$staged" || die "cannot write to $prefix"
    mv -f "$staged" "$dest" || die "cannot move the new binary into place at $dest"
    staged=
    if [ -n "$replacing" ]; then
      say "replaced $replacing with $ver at $dest"
    else
      say "installed $ver at $dest"
    fi
  fi
  current=$(bin_version "$dest")
  [ -n "$current" ] || note "$TOOL install: $dest does not run with --version; check that the file is executable here"
  path_hint
}

case $mode in
  install)   do_install ;;
  status)    do_status ;;
  uninstall) do_uninstall ;;
esac
