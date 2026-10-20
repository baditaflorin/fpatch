# Security

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's private
vulnerability reporting for this repository
(<https://github.com/baditaflorin/fpatch/security/advisories/new>) and include
what you did, what you expected, and what happened. You will get an answer as
soon as the maintainer can give one; this is a small project without a
guaranteed response time.

Supported versions: the latest release and `main`.

## What fpatch does and does not do

fpatch edits files. Its security properties come from doing little, and doing
it in a way that is easy to inspect.

### In every mode

- **Standard library only**, no network access of its own, no telemetry, no
  update checks, no files written other than the ones you name (plus the
  temporary file next to each and, with `--backup`, `FILE.bak`).
- **Paths come only from the command line.** The spec on stdin is treated as
  data. `=== file PATH` can only select one of the file arguments, byte for
  byte; it cannot add a path. A reviewer or a policy hook that looks at the
  command line sees every file that can be touched.
- Paths with control characters (newline, NUL, escape, ...) or invalid UTF-8
  are refused before anything runs.
- fpatch **never creates, deletes or renames** files you named (the one
  exception is the replace-by-rename of each edited file, and `FILE.bak` with
  `--backup`). A mistyped path is an error, not a new file.
- Files are replaced by writing a temporary file in the same directory
  (created with mode `0600`, then set to the original's mode) and renaming it.
  A failed write leaves the original and removes the temporary file.
- **Symlinks are followed.** If you name a symlink, its target is what gets
  patched (the link stays a link). Check what a path points at before running
  fpatch with elevated privileges on a directory others can write to; as with
  any tool that edits a path, an attacker who can swap a file for a symlink
  between your check and fpatch's run can redirect the edit. fpatch narrows
  that window (it refuses files that changed since it read them) but cannot
  close it.
- **Diffs and error messages quote file content.** A diff shows the changed
  lines with context; a miss prints the nearest lines of the file. If a file
  holds secrets and the output is logged (by an agent, by CI), use `--quiet`
  (which also removes diffs from `--json`); error messages may still quote a
  few lines near a miss.
- `--backup` leaves a copy of the previous content, with the same mode, next to
  the file. Remember it when the file holds secrets.
- Ownership, ACLs, extended attributes and SELinux labels are not preserved
  (ownership is restored only when permitted). The replacement is a new file.

### With `--host` (the ssh backend)

fpatch runs the system `ssh` binary and sends it commands. What that means:

**It does**

- Open **one** ssh connection per run, to the host you name, with `BatchMode=yes`
  (never prompts for a password or passphrase, never hangs on a question),
  `ClearAllForwardings=yes` (no port or agent forwarding from your config is
  activated for this session), and `RemoteCommand=none`. Host key checking is
  whatever your ssh configuration says; fpatch does not disable or relax it, and
  an unknown host fails rather than prompting.
- Run a **fixed helper script**, embedded in the binary, in a plain `sh` on the
  remote host. The script is in the source tree (`fsys/ssh_prelude.sh`); it
  uses `cat`, `cksum`, `chmod`, `chown`, `cp`, `dirname`, `head`, `mktemp`,
  `mv`, `readlink`, `rm`, `stat`, `tr` and `wc`, only on the files you named
  and on temporary files beside them.
- Pass paths and options as **single-quoted arguments** to functions of that
  script and file content as a **quoted here-document with a random delimiter
  that is verified not to occur in the content**. Nothing a spec or a file
  contains is ever evaluated by the remote shell. The quoting is covered by a
  property test and a fuzz target against a real shell.
- Validate the host alias (`[A-Za-z0-9_]` first, then letters, digits and
  `._@:%+=,~-`) and place it after `--`, so it cannot become an ssh option
  (for example `-oProxyCommand=...`).

**It does not**

- Install anything, leave anything running, or change the remote host's
  configuration.
- Touch paths other than the ones named on the command line (plus their
  temporary files and, with `--backup`, `FILE.bak`).
- Use `sudo`, change users, or request a terminal.
- Read your ssh keys or agent itself; that is ssh's job.

**It cannot protect you from**

- **A compromised or malicious remote host.** The remote side runs the helper,
  returns whatever it likes as file content, and could ignore the write. fpatch
  treats file content as data and checks what it can (sizes, framing, the
  per-run token), but a host you do not trust cannot be made safe by the client.
- **A hostile ssh configuration or `PATH`.** fpatch runs the first `ssh` on
  your `PATH` with your `~/.ssh/config`; anything that can alter those can alter
  what fpatch does, exactly as for running `ssh` by hand.
- **Races on the remote host.** The "file changed since it was read" check
  (POSIX `cksum`) closes most of the window between reading and writing, not
  all of it.
- A **login that prints a lot, or restricts commands** (`ForceCommand`,
  restricted shells): the first is tolerated, the second makes fpatch fail with
  ssh's message.

### Spec injection

An agent or script that builds a spec from untrusted text should remember that
the spec decides what is written into the files you named. fpatch limits *which
files* (the command line) and *how often* an edit may match (`count`), not what
the new text says. Review the diff (`--dry-run`) when the content comes from an
untrusted source.
