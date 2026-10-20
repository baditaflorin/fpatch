#!/usr/bin/env bash
# Release fpatch: build the archives on a build host (never on this machine), verify them here and show
# exactly what would be uploaded. A dry run by default; --publish then creates the GitHub release from
# THIS machine, so no GitHub token ever sits on the build host.
#
#   scripts/release.sh vX.Y.Z               guards, build on the host, verify here, print the upload plan
#   scripts/release.sh vX.Y.Z --publish     the same, then `gh release create` (you retype the tag to confirm)
#   scripts/release.sh vX.Y.Z --publish --yes   no confirmation question (for scripts)
#
# Environment:
#   BUILD_HOST    ssh alias or user@host of a Linux machine with docker (required; builds never run here)
#   GO_IMAGE      builder image (default golang:1.26)
#
# It refuses unless the working tree is clean, HEAD is origin/main, the tag does not exist yet, and the
# version agrees with CHANGELOG.md. docs/RELEASING.md has the whole procedure.
set -euo pipefail

TOOL=fpatch
REPO=baditaflorin/fpatch
HOST=${BUILD_HOST:-}
IMAGE=${GO_IMAGE:-golang:1.26}
MEMBERS="fpatch README.md LICENSE"
PLATFORMS="darwin_amd64 darwin_arm64 linux_amd64 linux_arm64"

tmp=
cleanup() { [ -z "$tmp" ] || rm -rf "$tmp"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

die() { printf 'release: %s\n' "$*" >&2; exit 1; }
usage_error() { printf 'release: %s\nusage: scripts/release.sh vX.Y.Z [--publish [--yes]]\n' "$*" >&2; exit 2; }
step() { printf '\n== %s\n' "$*"; }
ok() { printf '  ok   %s\n' "$*"; }
warn() { printf '  WARN %s\n' "$*"; }

# ---- arguments ---------------------------------------------------------------------------------

tag=
publish=0
yes=0
while [ $# -gt 0 ]; do
  case $1 in
    --publish) publish=1; shift ;;
    --yes)     yes=1; shift ;;
    -h|--help) awk 'NR == 1 { next } /^set -euo/ { exit } { sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
    -*)        usage_error "unknown option $1" ;;
    *)         [ -z "$tag" ] || usage_error "more than one version given"; tag=$1; shift ;;
  esac
done
[ -n "$tag" ] || usage_error "give the version to release, for example v0.1.0"
tag_re='^v[0-9]+\.[0-9]+\.[0-9]+$'
[[ $tag =~ $tag_re ]] || usage_error "the version must look like vX.Y.Z, got '$tag'"
ver=${tag#v}
[ "$yes" = 0 ] || [ "$publish" = 1 ] || usage_error "--yes only makes sense with --publish"
image_re='^[A-Za-z0-9._/:-]+$'
[[ $IMAGE =~ $image_re ]] || die "GO_IMAGE '$IMAGE' has characters that are not allowed"
host_re='^[A-Za-z0-9._@-]+$'
[ -n "$HOST" ] || die "BUILD_HOST is not set: give the ssh alias (or user@host) of a Linux machine with docker"
[[ $HOST =~ $host_re ]] || die "BUILD_HOST '$HOST' has characters that are not allowed"

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

# listed FILE NAME: does checksums.txt FILE have a line for exactly NAME?
listed() {
  awk -v n="$2" '$2 == n || $2 == "*" n { found = 1 } END { exit !found }' "$1"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

# ---- 1. guards (all local, nothing is built before they pass) -------------------------------------

# the version section of CHANGELOG.md: the lines between its heading and the next "## " heading
changelog_section() {
  awk -v v="$1" '
    /^## / {
      if (insec) exit
      h = $0; sub(/^## \[?/, "", h)
      if (index(h, v) == 1 && (length(h) == length(v) || substr(h, length(v) + 1, 1) ~ /[] ]/)) { insec = 1; next }
    }
    insec { print }' CHANGELOG.md
}

# the heading line of that section
changelog_heading() {
  awk -v v="$1" '
    /^## / { h = $0; sub(/^## \[?/, "", h)
             if (index(h, v) == 1 && (length(h) == length(v) || substr(h, length(v) + 1, 1) ~ /[] ]/)) { print; exit } }' CHANGELOG.md
}

# the first version heading (not "Unreleased") in CHANGELOG.md, as a bare X.Y.Z
changelog_top_version() {
  awk '
    /^## / { h = $0; sub(/^## \[?/, "", h)
             if (tolower(h) ~ /^unreleased/) next
             if (match(h, /^[0-9]+\.[0-9]+\.[0-9]+/)) { print substr(h, 1, RLENGTH) }
             exit }' CHANGELOG.md
}

# non-blank lines under "## [Unreleased]"
changelog_unreleased() {
  awk '
    /^## / { if (insec) exit; h = $0; sub(/^## \[?/, "", h); if (tolower(h) ~ /^unreleased/) insec = 1; next }
    insec && NF { print }' CHANGELOG.md
}

guards() {
  step "guards"
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "not inside a git work tree"
  [ -f CHANGELOG.md ] || die "no CHANGELOG.md in $root"

  local url slug
  url=$(git remote get-url origin 2>/dev/null) || die "this clone has no remote named origin"
  slug=$(printf '%s' "$url" | sed -e 's/\.git$//' -e 's#.*[:/]\([^/:]*/[^/:]*\)$#\1#')
  [ "$slug" = "$REPO" ] || die "origin is $slug, not $REPO: release from a clone of $REPO"
  ok "origin is $REPO"

  [ -z "$(git status --porcelain)" ] || die "the working tree is not clean (commit or remove: $(git status --porcelain | head -n 3 | tr '\n' ' '))"
  ok "working tree is clean"

  git fetch --quiet origin main || die "cannot fetch origin/main"
  target=$(git rev-parse HEAD)
  [ "$target" = "$(git rev-parse origin/main)" ] \
    || die "HEAD ($(git rev-parse --short HEAD)) is not origin/main ($(git rev-parse --short origin/main)): release from an up-to-date main"
  ok "HEAD is origin/main ($(git rev-parse --short HEAD))"

  if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then die "tag $tag already exists locally"; fi
  # ls-remote exits 2 when the tag does not exist, and 128 when the remote cannot be reached
  local rc=0
  git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null 2>&1 || rc=$?
  [ "$rc" -ne 0 ] || die "tag $tag already exists on origin"
  [ "$rc" -eq 2 ] || die "cannot reach origin to check for tag $tag"
  ok "tag $tag does not exist yet"

  local top heading
  top=$(changelog_top_version)
  [ "$top" = "$ver" ] || die "the newest version in CHANGELOG.md is '${top:-none}', not $ver"
  heading=$(changelog_heading "$ver")
  [ -n "$(changelog_section "$ver" | awk 'NF')" ] || die "the CHANGELOG.md section for $ver is empty"
  if [ -n "$(changelog_unreleased)" ]; then
    die "CHANGELOG.md has entries under Unreleased: move them under $ver first"
  fi
  case $(printf '%s' "$heading" | tr '[:upper:]' '[:lower:]') in
    *unreleased*|*"not yet released"*)
      if [ "$publish" = 1 ]; then
        die "the CHANGELOG.md heading still says '${heading#\#\# }': put the release date there, commit and merge"
      fi
      warn "the CHANGELOG.md heading still says '${heading#\#\# }' (--publish would refuse until it has a date)" ;;
  esac
  ok "CHANGELOG.md: newest version is $ver"
}

# ---- 2. the build host -----------------------------------------------------------------------------

check_host() {
  step "build host $HOST"
  local info load cores free_kb
  info=$(ssh -o BatchMode=yes "$HOST" 'uptime; nproc; df -Pk /tmp | tail -n 1') || die "cannot reach $HOST over ssh"
  load=$(printf '%s\n' "$info" | sed -n '1s/.*load averages*: *\([0-9.]*\).*/\1/p')
  cores=$(printf '%s\n' "$info" | sed -n '2p')
  free_kb=$(printf '%s\n' "$info" | awk 'NR == 3 { print $4 }')
  printf '  load %s on %s cores, %s MB free in /tmp\n' "${load:-?}" "${cores:-?}" "$(( ${free_kb:-0} / 1024 ))"
  [ "${free_kb:-0}" -ge 1048576 ] || die "less than 1 GB free in /tmp on $HOST: pick another BUILD_HOST, or clean up there"
  if [ -n "$load" ] && [ -n "$cores" ] && awk -v l="$load" -v c="$cores" 'BEGIN { exit !(l > 4 * c) }'; then
    warn "$HOST is busy (load $load on $cores cores); the build will be slow. Set BUILD_HOST to a quieter machine if this is too slow."
  fi
}

# ---- 3. build there, bring dist/ back --------------------------------------------------------------

# the script the build host runs: the tree arrives on stdin as a tar stream, dist/ leaves on stdout
# as a tar stream (everything else the build prints goes to stderr). The container runs as root, so it
# gives dist/ back to the host user before it exits; then the host's rm can remove it.
build_remote() {
  local script
  IFS= read -r -d '' script <<'REMOTE' || true
set -euo pipefail
d=$(mktemp -d /tmp/fpatch-release.XXXXXX)
trap "rm -rf $d" EXIT
mkdir $d/src
tar -x -C $d/src
docker run --rm --init --cpus 4 --cpu-shares 256 -v fpatch-gocache:/root/.cache/go-build -v $d/src:/src -w /src -e GOTOOLCHAIN=local -e GOFLAGS=-buildvcs=false -e HOST_UID=$(id -u) -e HOST_GID=$(id -g) @IMAGE@ sh -c "trap \"chown -R \$HOST_UID:\$HOST_GID /src\" EXIT; make dist VERSION=@TAG@ >&2 && tar -C dist -c ."
REMOTE
  script=${script//@IMAGE@/$IMAGE}
  script=${script//@TAG@/$tag}
  case $script in *"'"*) die "internal error: the remote script must not contain a single quote" ;; esac
  git archive --format=tar "$target" | ssh -o BatchMode=yes "$HOST" "bash -c '$script'"
}

build() {
  step "build $TOOL $tag on $HOST (docker run --rm --init $IMAGE; one ssh connection)"
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/$TOOL-release.XXXXXX")
  mkdir "$tmp/dist"
  build_remote | tar -x -C "$tmp/dist" || die "the build on $HOST failed (see the output above)"
  ok "dist/ came back: $(find "$tmp/dist" -type f | wc -l | tr -d ' ') files"
}

# the verified build replaces dist/ in the working tree; a build that fails verification never does
keep_dist() {
  rm -rf "$root/dist"
  mv "$tmp/dist" "$root/dist"
  chmod 755 "$root/dist"
  ok "dist/ in the working tree is the verified build"
}

# ---- 4. verify what came back ----------------------------------------------------------------------

verify_dist() {
  step "verify dist/ against checksums.txt"
  local dir=$1 sums=$1/checksums.txt n=0 want f got p m extra
  [ -s "$sums" ] || die "dist/checksums.txt is missing or empty"
  local sum_re='^[0-9a-f]{64}$'
  while read -r want f; do
    f=${f#\*}
    [[ $want =~ $sum_re ]] || die "checksums.txt has a malformed line for '$f'"
    case $f in
      "${TOOL}_${ver}_"*.tar.gz) ;;
      *) die "checksums.txt lists '$f', which is not a $TOOL $ver archive" ;;
    esac
    [ -f "$dir/$f" ] || die "checksums.txt lists $f but the file is not in dist/"
    got=$(sha256_of "$dir/$f")
    [ "$got" = "$want" ] || die "checksum mismatch for $f: checksums.txt says $want, the file is $got"
    n=$((n + 1))
  done <"$sums"
  for p in $PLATFORMS; do
    listed "$sums" "${TOOL}_${ver}_${p}.tar.gz" || die "checksums.txt has no entry for ${TOOL}_${ver}_${p}.tar.gz"
  done
  [ "$n" -eq 4 ] || die "checksums.txt lists $n archives, expected 4"
  extra=$(cd "$dir" && for f in "${TOOL}"_*.tar.gz; do listed checksums.txt "$f" || printf '%s ' "$f"; done)
  [ -z "$extra" ] || die "dist/ holds archives that checksums.txt does not list: $extra"
  ok "4 archives, every sha256 matches checksums.txt"

  for p in $PLATFORMS; do
    tar -tzf "$dir/${TOOL}_${ver}_${p}.tar.gz" >"$tmp/list" || die "${TOOL}_${ver}_${p}.tar.gz is not a readable tar.gz"
    for m in $MEMBERS; do
      grep -qx "$m" "$tmp/list" || die "${TOOL}_${ver}_${p}.tar.gz does not contain $m"
    done
  done
  ok "every archive holds: $MEMBERS"

  # the binary for this machine must print the bare version (the installer relies on it)
  local os arch native out
  case $(uname -s) in Darwin) os=darwin ;; Linux) os=linux ;; *) os= ;; esac
  case $(uname -m) in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) arch= ;; esac
  if [ -n "$os" ] && [ -n "$arch" ]; then
    native=$dir/${TOOL}_${ver}_${os}_${arch}.tar.gz
    mkdir "$tmp/native"
    tar -xzf "$native" -C "$tmp/native" "$TOOL" || die "cannot extract $TOOL from $native"
    out=$("$tmp/native/$TOOL" --version 2>&1) || die "the $os/$arch binary does not run: $out"
    [ "$out" = "$ver" ] || die "the $os/$arch binary prints '$out' for --version, expected exactly '$ver'"
    ok "$os/$arch binary: --version prints $ver"
  else
    warn "no native archive for $(uname -s)/$(uname -m): the --version check was skipped"
  fi
}

# ---- 5. the plan, and --publish --------------------------------------------------------------------

assets() {
  local p
  for p in $PLATFORMS; do printf '%s\n' "dist/${TOOL}_${ver}_${p}.tar.gz"; done
  printf '%s\n' dist/checksums.txt install.sh
}

print_plan() {
  local f
  step "upload plan for $REPO $tag"
  printf '  repository : %s\n' "$REPO"
  printf '  tag        : %s (created by gh at %s)\n' "$tag" "$target"
  printf '  title      : %s %s\n' "$TOOL" "$tag"
  printf '  assets     :\n'
  for f in $(assets); do
    printf '    %-48s %9s bytes  sha256 %s\n' "$f" "$(wc -c <"$f" | tr -d ' ')" "$(sha256_of "$f")"
  done
  printf '  notes      : the %s lines of the %s section of CHANGELOG.md, below\n' "$(wc -l <"$tmp/notes.md" | tr -d ' ')" "$ver"
  sed 's/^/    | /' "$tmp/notes.md"
  printf '  command    : '
  printf '%q ' gh release create "$tag" --repo "$REPO" --target "$target" --title "$TOOL $tag" --notes-file "$tmp/notes.md"
  for f in $(assets); do printf '%s ' "$f"; done
  printf '\n'
}

do_publish() {
  step "publish"
  command -v gh >/dev/null 2>&1 || die "gh (the GitHub CLI) is needed to publish"
  gh auth status >/dev/null 2>&1 || die "gh is not logged in (gh auth login)"
  if gh release view "$tag" --repo "$REPO" >/dev/null 2>&1; then die "a release for $tag already exists on $REPO"; fi
  if [ "$yes" = 0 ]; then
    [ -t 0 ] || die "no terminal to ask on: add --yes to publish non-interactively"
    printf 'Type %s to create the release on %s: ' "$tag" "$REPO"
    local answer
    read -r answer
    [ "$answer" = "$tag" ] || die "not confirmed, nothing was published"
  fi
  # shellcheck disable=SC2046  # the asset paths have no spaces
  gh release create "$tag" --repo "$REPO" --target "$target" --title "$TOOL $tag" --notes-file "$tmp/notes.md" $(assets)
  ok "published $tag: https://github.com/$REPO/releases/tag/$tag"
}

target=
guards
check_host
build
verify_dist "$tmp/dist"
keep_dist
changelog_section "$ver" | sed -e '/./,$!d' >"$tmp/notes.md"
print_plan
if [ "$publish" = 1 ]; then
  do_publish
else
  printf '\nDRY RUN: nothing was uploaded and no tag was created. dist/ is built and verified; add --publish to release it.\n'
fi
