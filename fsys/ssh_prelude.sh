# fpatch remote helper, sent once per run to a POSIX sh on the remote host.
#
# Rules this file follows:
#   * POSIX sh only (runs under dash, bash, busybox ash and macOS /bin/sh), and
#     only tools found on GNU/Linux, busybox and BSD/macOS: cat, cksum, chmod,
#     chown, cp, dirname, head, mktemp, mv, readlink, rm, stat, tr, wc.
#   * FPT, a random per-run token, is set by the caller before this text.
#   * Nothing in here is built from a user-supplied value. Paths and options
#     arrive as arguments of fp_read / fp_write, single-quoted by the caller,
#     and file content arrives as a quoted here-document.
#   * Every reply starts on a fresh line with the token, so noise from the
#     remote login scripts cannot be mistaken for a reply.
#   * Failures never exit the shell: they are reported as "err CODE MESSAGE" so
#     the session (one ssh connection) survives for the next request.

fp_say() { printf '\n%s %s\n' "$FPT" "$*"; }

fp_err() {
  fp_ec=$1
  shift
  fp_em=$(printf '%s' "$*" | tr '\n\r' '  ')
  fp_say "err $fp_ec $fp_em"
}

fp_hello() { fp_say hello; }

# fp_res PATH: resolve symlinks (up to 40 hops) into FP_REAL.
fp_res() {
  FP_REAL=$1
  fp_n=0
  while [ -L "$FP_REAL" ]; do
    fp_n=$((fp_n + 1))
    if [ "$fp_n" -gt 40 ]; then
      return 1
    fi
    fp_l=$(readlink "$FP_REAL") || return 1
    case $fp_l in
      /*) FP_REAL=$fp_l ;;
      *) FP_REAL=$(dirname "$FP_REAL")/$fp_l ;;
    esac
  done
  return 0
}

fp_mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1" 2>/dev/null; }

fp_owner() { stat -c %u:%g "$1" 2>/dev/null || stat -f %u:%g "$1" 2>/dev/null; }

# fp_ck FILE: "CRC SIZE" as printed by POSIX cksum, whatever the spacing.
fp_ck() {
  set -- $(cksum < "$1" 2>/dev/null)
  printf '%s %s' "$1" "$2"
}

# fp_read PATH: reply "ok MODE", the file content, then "end STATUS"; or one of
# notfound / notregular / denied / err.
fp_read() {
  fp_p=$1
  case $fp_p in
    /*) ;;
    *) fp_p=./$fp_p ;;
  esac
  if ! fp_res "$fp_p"; then
    fp_err io "cannot resolve symlinks of $fp_p"
    return 0
  fi
  if [ ! -e "$FP_REAL" ]; then
    fp_say notfound
    return 0
  fi
  if [ ! -f "$FP_REAL" ]; then
    fp_say notregular
    return 0
  fi
  if [ ! -r "$FP_REAL" ]; then
    fp_say denied
    return 0
  fi
  fp_mo=$(fp_mode "$FP_REAL")
  fp_say "ok $fp_mo"
  cat < "$FP_REAL" 2>/dev/null
  fp_rc=$?
  printf '\n%s end %s\n' "$FPT" "$fp_rc"
}

fp_clean() {
  if [ -n "$fp_s" ]; then rm -f "$fp_s"; fi
  if [ -n "$fp_t" ]; then rm -f "$fp_t"; fi
  fp_s=
  fp_t=
}

# fp_write PATH SIZE EXPECT BACKUP-SUFFIX: replace the content of an existing
# regular file with the here-document on stdin. SIZE is the content length;
# the here-document holds the content plus a final newline when the content
# does not end in one. EXPECT, when not empty, is the "CRC SIZE" the file must
# still have. Reply "done", or "err CODE MESSAGE".
fp_write() {
  fp_p=$1
  fp_size=$2
  fp_expect=$3
  fp_bak=$4
  fp_s=
  fp_t=
  case $fp_p in
    /*) ;;
    *) fp_p=./$fp_p ;;
  esac
  if ! fp_res "$fp_p"; then
    fp_err io "cannot resolve symlinks of $fp_p"
    return 0
  fi
  fp_real=$FP_REAL
  if [ ! -e "$fp_real" ]; then
    fp_err notfound "$fp_p"
    return 0
  fi
  if [ ! -f "$fp_real" ]; then
    fp_err notregular "$fp_p"
    return 0
  fi
  if [ -n "$fp_expect" ]; then
    if [ "$(fp_ck "$fp_real")" != "$fp_expect" ]; then
      fp_err conflict "$fp_p"
      return 0
    fi
  fi
  fp_dir=$(dirname "$fp_real")
  if ! fp_mt=$(mktemp "$fp_dir/.fpatch.XXXXXX" 2>&1); then
    fp_err io "cannot create a temporary file in $fp_dir: $fp_mt"
    return 0
  fi
  fp_s=$fp_mt
  if ! cat > "$fp_s"; then
    fp_clean
    fp_err io "cannot stage the new content in $fp_dir"
    return 0
  fi
  set -- $(wc -c < "$fp_s")
  fp_got=$1
  if [ "$fp_got" = "$fp_size" ]; then
    fp_t=$fp_s
    fp_s=
  elif [ "$fp_got" = "$((fp_size + 1))" ]; then
    if ! fp_mt=$(mktemp "$fp_dir/.fpatch.XXXXXX" 2>&1); then
      fp_clean
      fp_err io "cannot create a temporary file in $fp_dir: $fp_mt"
      return 0
    fi
    fp_t=$fp_mt
    if ! head -c "$fp_size" "$fp_s" > "$fp_t"; then
      fp_clean
      fp_err io "cannot trim the staged content"
      return 0
    fi
    rm -f "$fp_s"
    fp_s=
  else
    fp_clean
    fp_err io "transfer damaged: expected $fp_size bytes, staged $fp_got"
    return 0
  fi
  fp_mo=$(fp_mode "$fp_real")
  case $fp_mo in
    [0-7][0-7][0-7] | [0-7][0-7][0-7][0-7]) ;;
    *)
      fp_clean
      fp_err io "cannot determine the mode of $fp_p (needs GNU or BSD stat)"
      return 0
      ;;
  esac
  fp_ow=$(fp_owner "$fp_real")
  if [ -n "$fp_ow" ]; then
    chown "$fp_ow" "$fp_t" 2>/dev/null
  fi
  if ! chmod "$fp_mo" "$fp_t"; then
    fp_clean
    fp_err io "cannot set the mode of the new file"
    return 0
  fi
  if [ -n "$fp_bak" ]; then
    if [ -e "$fp_p$fp_bak" ] && [ ! -f "$fp_p$fp_bak" ]; then
      fp_clean
      fp_err io "the backup path $fp_p$fp_bak exists and is not a regular file"
      return 0
    fi
    if [ -L "$fp_p$fp_bak" ]; then
      rm -f "$fp_p$fp_bak"
    fi
    if ! fp_o=$(cp -p "$fp_real" "$fp_p$fp_bak" 2>&1); then
      fp_clean
      fp_err io "cannot write the backup $fp_p$fp_bak: $fp_o"
      return 0
    fi
  fi
  if ! fp_o=$(mv -f "$fp_t" "$fp_real" 2>&1); then
    fp_clean
    fp_err io "cannot replace $fp_p: $fp_o"
    return 0
  fi
  fp_say done
}
