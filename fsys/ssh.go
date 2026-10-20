package fsys

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The ssh backend talks to a POSIX sh on the remote host over ONE ssh
// connection per run. It starts the system ssh binary (so ~/.ssh/config,
// ProxyJump and ControlMaster multiplexing keep working and nothing needs to
// be installed remotely), sends the helper functions of ssh_prelude.sh once,
// and then issues one request per Read or Write. See docs/ARCHITECTURE.md for
// the wire format and SECURITY.md for what a remote run can and cannot do.

//go:embed ssh_prelude.sh
var sshPrelude string

func init() {
	Register("ssh", func(alias string) (FS, error) { return newSSH(alias) })
}

// aliasRE is deliberately narrow: ssh_config host aliases, host names,
// user@host and IP literals. A leading "-" is impossible, so an alias can never
// be parsed by ssh as an option.
var aliasRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@:%+=,~-]*$`)

var versionRE = regexp.MustCompile(`^[0-9]+ [0-9]+$`)

type sshFS struct {
	alias string

	mu      sync.Mutex // serialises requests and guards the fields below
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	pr      *io.PipeReader
	stdout  *bufio.Reader
	stderr  *tailBuffer
	token   string
	waited  chan struct{} // closed once cmd.Wait has returned
	waitErr error
	broken  error // sticky: the session is unusable
	closed  bool

	killOnce sync.Once
}

func newSSH(alias string) (FS, error) {
	if alias == "" {
		return nil, errors.New("ssh needs a host alias, for example --host web1")
	}
	if !aliasRE.MatchString(alias) {
		return nil, fmt.Errorf("invalid ssh host alias %q: use a name from your ssh config or a host name", alias)
	}
	return &sshFS{alias: alias, stderr: &tailBuffer{}}, nil
}

// sshArgs is the full argument list for the one ssh process of a run. The
// alias comes after "--" so it can never be taken for an option.
func sshArgs(alias string) []string {
	return []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "RemoteCommand=none",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"--", alias, "sh",
	}
}

// shQuote single-quotes s for a POSIX shell. It is only ever applied to
// values that already passed ValidatePath (no NUL, no control characters).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 8192 {
		t.b = t.b[len(t.b)-8192:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// abort kills the ssh process; it is safe to call from any goroutine, any
// number of times, and does not need s.mu (a request in flight holds it).
func (s *sshFS) abort() {
	s.killOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		if s.pr != nil {
			_ = s.pr.Close()
		}
	})
}

// start launches ssh and waits for the helper to answer. Callers hold s.mu.
func (s *sshFS) start(ctx context.Context) error {
	if s.broken != nil {
		return s.broken
	}
	if s.closed {
		return errors.New("ssh file system is closed")
	}
	if s.cmd != nil {
		return nil
	}
	bin, err := exec.LookPath("ssh")
	if err != nil {
		s.broken = fmt.Errorf("cannot run ssh: %w", err)
		return s.broken
	}
	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return err
	}
	s.token = hex.EncodeToString(tok[:])

	cmd := exec.Command(bin, sshArgs(s.alias)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		s.broken = fmt.Errorf("cannot start ssh: %w", err)
		return s.broken
	}
	s.cmd, s.stdin, s.pr = cmd, stdin, pr
	s.stdout = bufio.NewReaderSize(pr, 64<<10)
	s.waited = make(chan struct{})
	go func() {
		s.waitErr = cmd.Wait()
		pw.Close()
		close(s.waited)
	}()

	stop := context.AfterFunc(ctx, s.abort)
	defer stop()
	hello := "FPT=" + s.token + "\n" + sshPrelude + "\nfp_hello\n"
	if _, err := io.WriteString(s.stdin, hello); err != nil {
		return s.fail(ctx, err)
	}
	kind, _, err := s.readReply()
	if err != nil {
		return s.fail(ctx, err)
	}
	if kind != "hello" {
		s.abort()
		s.broken = fmt.Errorf("ssh %s: unexpected answer %q from the remote shell", s.alias, kind)
		return s.broken
	}
	return nil
}

// fail turns a broken connection into a sticky error that carries ssh's own
// message (permission denied, could not resolve host, ...).
func (s *sshFS) fail(ctx context.Context, cause error) error {
	select {
	case <-s.waited:
	case <-time.After(2 * time.Second):
		s.abort()
		<-s.waited
	}
	if err := ctx.Err(); err != nil {
		s.broken = fmt.Errorf("ssh %s: %w", s.alias, err)
		return s.broken
	}
	msg := s.stderr.String()
	switch {
	case errors.Is(cause, ErrTooLarge):
		s.broken = fmt.Errorf("ssh %s: %w", s.alias, ErrTooLarge)
	case msg != "":
		s.broken = fmt.Errorf("ssh %s: %s", s.alias, strings.ReplaceAll(msg, "\n", "; "))
	case s.waitErr != nil:
		s.broken = fmt.Errorf("ssh %s: connection ended: %v", s.alias, s.waitErr)
	default:
		s.broken = fmt.Errorf("ssh %s: connection ended unexpectedly: %v", s.alias, cause)
	}
	return s.broken
}

// readLine reads one line of at most limit bytes.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > limit {
			return nil, ErrTooLarge
		}
		if err == nil {
			return out, nil
		}
		if err != bufio.ErrBufferFull {
			return out, err
		}
	}
}

// readReply skips anything the remote login scripts printed and returns the
// next reply line: its kind (the word after the token) and the rest.
func (s *sshFS) readReply() (kind, rest string, err error) {
	prefix := s.token + " "
	for {
		line, err := readLine(s.stdout, 1<<20)
		if err != nil {
			return "", "", err
		}
		text := strings.TrimRight(string(line), "\r\n")
		if strings.HasPrefix(text, prefix) {
			kind, rest, _ = strings.Cut(text[len(prefix):], " ")
			return kind, rest, nil
		}
	}
}

// readBody reads file content up to the "end" line.
func (s *sshFS) readBody() ([]byte, error) {
	endPrefix := []byte(s.token + " end ")
	var buf []byte
	for {
		line, err := readLine(s.stdout, MaxFileSize+2)
		if err != nil {
			return nil, err
		}
		if bytes.HasPrefix(line, endPrefix) {
			status := strings.TrimSpace(string(line[len(endPrefix):]))
			if len(buf) == 0 || buf[len(buf)-1] != '\n' {
				return nil, errors.New("malformed reply from the remote shell")
			}
			if status != "0" {
				return nil, fmt.Errorf("reading failed on the remote host (status %s)", status)
			}
			return buf[:len(buf)-1], nil
		}
		buf = append(buf, line...)
		if len(buf) > MaxFileSize+1 {
			return nil, ErrTooLarge
		}
	}
}

func (s *sshFS) Read(ctx context.Context, path string) (File, error) {
	if err := ValidatePath(path); err != nil {
		return File{}, err
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.start(ctx); err != nil {
		return File{}, err
	}
	stop := context.AfterFunc(ctx, s.abort)
	defer stop()

	if _, err := io.WriteString(s.stdin, "fp_read "+shQuote(path)+"\n"); err != nil {
		return File{}, s.fail(ctx, err)
	}
	kind, rest, err := s.readReply()
	if err != nil {
		return File{}, s.fail(ctx, err)
	}
	switch kind {
	case "ok":
		data, err := s.readBody()
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				s.abort()
				return File{}, s.fail(ctx, err)
			}
			return File{}, fmt.Errorf("%s: %w", path, err)
		}
		mode, _ := strconv.ParseUint(strings.TrimSpace(rest), 8, 32)
		return File{Data: data, Version: sshVersion(data), Mode: fs.FileMode(mode).Perm()}, nil
	case "notfound":
		return File{}, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	case "notregular":
		return File{}, fmt.Errorf("%s: %w", path, ErrNotRegular)
	case "denied":
		return File{}, fmt.Errorf("%s: %w", path, fs.ErrPermission)
	case "err":
		return File{}, s.remoteErr(path, rest)
	}
	s.abort()
	s.broken = fmt.Errorf("ssh %s: unexpected reply %q", s.alias, kind)
	return File{}, s.broken
}

func (s *sshFS) Write(ctx context.Context, path string, data []byte, opts WriteOptions) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	if opts.BackupSuffix != "" {
		if err := ValidatePath(path + opts.BackupSuffix); err != nil {
			return err
		}
	}
	if opts.ExpectVersion != "" && !versionRE.MatchString(opts.ExpectVersion) {
		return fmt.Errorf("invalid version token %q", opts.ExpectVersion)
	}
	if len(data) > MaxFileSize {
		return fmt.Errorf("%s: %w", path, ErrTooLarge)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("%s: content has a NUL byte, which the ssh backend cannot transfer", path)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.start(ctx); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, s.abort)
	defer stop()

	delim := heredocDelimiter(data)
	body := data
	if len(data) > 0 && data[len(data)-1] != '\n' {
		body = append(append(make([]byte, 0, len(data)+1), data...), '\n')
	}
	var req bytes.Buffer
	fmt.Fprintf(&req, "fp_write %s %d %s %s <<'%s'\n", shQuote(path), len(data), shQuote(opts.ExpectVersion), shQuote(opts.BackupSuffix), delim)
	req.Write(body)
	req.WriteString(delim + "\n")
	if _, err := s.stdin.Write(req.Bytes()); err != nil {
		return s.fail(ctx, err)
	}
	kind, rest, err := s.readReply()
	if err != nil {
		return s.fail(ctx, err)
	}
	switch kind {
	case "done":
		return nil
	case "err":
		return s.remoteErr(path, rest)
	}
	s.abort()
	s.broken = fmt.Errorf("ssh %s: unexpected reply %q", s.alias, kind)
	return s.broken
}

// remoteErr maps an "err CODE MESSAGE" reply to a Go error.
func (s *sshFS) remoteErr(path, rest string) error {
	code, msg, _ := strings.Cut(rest, " ")
	switch code {
	case "notfound":
		return fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	case "notregular":
		return fmt.Errorf("%s: %w", path, ErrNotRegular)
	case "conflict":
		return fmt.Errorf("%s: %w", path, ErrConflict)
	}
	return fmt.Errorf("%s: %s", path, msg)
}

func (s *sshFS) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.cmd == nil {
		return nil
	}
	if s.broken == nil {
		_, _ = io.WriteString(s.stdin, "exit\n")
	}
	_ = s.stdin.Close()
	select {
	case <-s.waited:
	case <-time.After(5 * time.Second):
		s.abort()
		<-s.waited
	}
	return nil
}

// heredocDelimiter returns a here-document delimiter that does not occur
// anywhere in data, so the body can be sent verbatim.
func heredocDelimiter(data []byte) string {
	for {
		var r [8]byte
		if _, err := rand.Read(r[:]); err != nil {
			panic(err) // crypto/rand does not fail on supported platforms
		}
		d := "FPATCH_EOF_" + hex.EncodeToString(r[:])
		if !bytes.Contains(data, []byte(d)) {
			return d
		}
	}
}

// sshVersion is the "CRC SIZE" pair POSIX cksum prints, which the remote side
// can recompute to detect a concurrent change.
func sshVersion(data []byte) string {
	return fmt.Sprintf("%d %d", posixCksum(data), len(data))
}

var cksumTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

// posixCksum is the CRC of POSIX cksum(1).
func posixCksum(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = crc<<8 ^ cksumTable[byte(crc>>24)^b]
	}
	for n := uint64(len(data)); n != 0; n >>= 8 {
		crc = crc<<8 ^ cksumTable[byte(crc>>24)^byte(n)]
	}
	return ^crc
}
