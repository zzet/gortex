package source

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// oidPattern is the only shape of object id this package will hand to
// git. Enforcing it before every invocation is what makes the git
// command lines safe: an argument that matches it cannot be an option,
// a ref, a path, or a revision expression with side effects.
var oidPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// batchShutdownGrace is how long Close waits for the batch child to
// exit on its own after its stdin is closed before killing it. git
// leaves the loop and exits as soon as it sees EOF, so the timer only
// matters for a child that is wedged.
const batchShutdownGrace = 2 * time.Second

// stderrCap bounds how much of a git child's stderr is kept for error
// messages.
const stderrCap = 4 << 10

// gitEnv returns the environment for every git child this package
// spawns.
//
// GIT_NO_LAZY_FETCH=1 is the one that matters: in a partial clone, a
// missing object would otherwise send git to the network mid-read,
// turning an indexing pass into an unbounded download. Together with
// using only plumbing commands that cannot fetch, it makes "no network,
// ever" a property of the process rather than a promise. Older git
// releases ignore the variable, which is harmless — they have no lazy
// fetch to disable. GIT_TERMINAL_PROMPT=0 keeps a credential prompt
// from blocking a daemon that has no terminal, and GIT_OPTIONAL_LOCKS=0
// keeps read-only commands from taking index locks.
var fixedGitEnv = []string{
	"GIT_NO_LAZY_FETCH=1",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_OPTIONAL_LOCKS=0",
}

func gitEnv() []string {
	return gitEnvFrom(os.Environ())
}

func gitEnvFrom(base []string) []string {
	env := make([]string, 0, len(base)+len(fixedGitEnv))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && isFixedGitEnvKey(key) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, fixedGitEnv...)
}

func isFixedGitEnvKey(key string) bool {
	for _, entry := range fixedGitEnv {
		fixedKey, _, _ := strings.Cut(entry, "=")
		if key == fixedKey {
			return true
		}
	}
	return false
}

// runGit runs one plumbing command in dir and returns its stdout. On
// failure the error wraps git's own stderr and the underlying
// *exec.ExitError, so callers can read the exit status.
//
// This does not go through internal/gitcmd: that helper owns the global
// git concurrency limiter but offers no way to set the child's
// environment, and every git child here must carry gitEnv.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv()
	platform.ConfigureBackgroundCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.Bytes(), fmt.Errorf("git %s: %w", args[0], ctxErr)
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// gitExitCode returns the exit status of a failed runGit call, or -1
// when the command never ran or was killed by a signal. git's plumbing
// convention separates "the object does not resolve" (1) from "this is
// not a repository" or "bad usage" (128, 129), which is how object
// absence is told apart from a broken setup.
func gitExitCode(err error) int {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}

// gitBatch is one long-lived `git cat-file` child that answers blob
// reads over a pipe.
//
// The whole point is that it is long-lived: a repository-sized indexing
// pass reads thousands of blobs, and paying a process spawn per blob
// costs more than everything else the pass does. One child per source,
// reused for every read, with the protocol serialized by the owner's
// mutex.
//
// Two protocol dialects are spoken. The preferred one is
// `--batch-command -Z`, where both the commands written and the
// responses read are NUL-delimited. The fallback for a git too old for
// -Z is plain `--batch`, which is newline-delimited on both sides and
// therefore only safe to feed pre-validated hex object ids — never a
// path or a ref, which can contain a newline and would let the input
// framing be forged.
type gitBatch struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	stderr *syncBuffer

	// delim terminates both a request and a response header: NUL in
	// --batch-command -Z mode, newline in the --batch fallback.
	delim byte
	// command is true in --batch-command mode, where a request is a
	// verb plus an object id rather than a bare object id.
	command bool

	// broken records the failure that desynchronized the pipe. Once the
	// protocol has lost its place the stream cannot be trusted again,
	// so every later read fails with the same error instead of
	// returning bytes from the wrong response.
	broken error
	closed bool
}

// probeBatchCommandZ reports whether the installed git understands
// `cat-file --batch-command -Z`. The probe runs the real command with
// an immediately-closed stdin: a git that supports the options reads
// EOF and exits 0, and one that does not fails its option parsing. It
// costs one short-lived process per source, paid once, so that every
// blob read afterwards can share a single child.
func probeBatchCommandZ(ctx context.Context, repoDir string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "cat-file", "--batch-command", "-Z")
	cmd.Env = gitEnv()
	platform.ConfigureBackgroundCommand(cmd)
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// startGitBatch spawns the batch child for repoDir.
//
// The child is deliberately not tied to a context: it outlives the call
// that first needed it and belongs to the source, whose Close is what
// terminates it.
func startGitBatch(ctx context.Context, repoDir string) (*gitBatch, error) {
	b := &gitBatch{stderr: &syncBuffer{limit: stderrCap}}
	args := []string{"-C", repoDir, "cat-file"}
	if probeBatchCommandZ(ctx, repoDir) {
		b.command = true
		b.delim = 0
		args = append(args, "--batch-command", "-Z")
	} else {
		b.delim = '\n'
		args = append(args, "--batch")
	}

	cmd := exec.Command("git", args...)
	cmd.Env = gitEnv()
	platform.ConfigureBackgroundCommand(cmd)
	cmd.Stderr = b.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git cat-file: start: %w", err)
	}
	b.cmd = cmd
	b.stdin = stdin
	b.out = bufio.NewReaderSize(stdout, 64<<10)
	return b, nil
}

// read returns the full content of the object named by oid.
//
// The blob is materialized in memory. That is the deliberate trade for
// a streaming reader: the pipe carries one response at a time, so
// handing a caller a reader over the live pipe would block every other
// read until that caller finished. Callers cap the size of what they
// index, so the peak is one admitted file.
//
// A "missing" response — the object is named by the tree but its bytes
// are not in this repository — is returned as ErrObjectMissing and
// leaves the protocol in a good state.
//
// The caller must hold the owning source's lock: the pipe carries one
// request and one response at a time.
func (b *gitBatch) read(oid string) ([]byte, error) {
	if b.broken != nil {
		return nil, b.broken
	}
	if !oidPattern.MatchString(oid) {
		return nil, fmt.Errorf("git cat-file: refusing to request %q: not an object id", oid)
	}

	req := make([]byte, 0, len(oid)+16)
	if b.command {
		req = append(req, "contents "...)
	}
	req = append(req, oid...)
	req = append(req, b.delim)
	if _, err := b.stdin.Write(req); err != nil {
		return nil, b.fail(fmt.Errorf("git cat-file: write request: %w%s", err, b.stderrSuffix()))
	}

	header, err := b.out.ReadString(b.delim)
	if err != nil {
		return nil, b.fail(fmt.Errorf("git cat-file: read response: %w%s", err, b.stderrSuffix()))
	}
	fields := strings.Fields(header[:len(header)-1])
	switch {
	case len(fields) == 2 && (fields[1] == "missing" || fields[1] == "ambiguous"):
		return nil, fmt.Errorf("git object %s: %w", oid, ErrObjectMissing)
	case len(fields) != 3:
		return nil, b.fail(fmt.Errorf("git cat-file: unexpected response header %q", header))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return nil, b.fail(fmt.Errorf("git cat-file: unexpected object size in %q", header))
	}

	buf := make([]byte, size)
	if _, err := io.ReadFull(b.out, buf); err != nil {
		return nil, b.fail(fmt.Errorf("git cat-file: read %d bytes of %s: %w%s", size, oid, err, b.stderrSuffix()))
	}
	trailer, err := b.out.ReadByte()
	if err != nil {
		return nil, b.fail(fmt.Errorf("git cat-file: read delimiter after %s: %w", oid, err))
	}
	if trailer != b.delim {
		return nil, b.fail(fmt.Errorf("git cat-file: missing delimiter after %s", oid))
	}
	return buf, nil
}

// fail records err as the failure that desynchronized the protocol and
// returns it, so the caller can `return nil, b.fail(err)`.
func (b *gitBatch) fail(err error) error {
	b.broken = err
	return err
}

// stderrSuffix renders what git complained about, if anything, for
// attaching to an I/O error.
func (b *gitBatch) stderrSuffix() string {
	msg := strings.TrimSpace(b.stderr.String())
	if msg == "" {
		return ""
	}
	return ": " + msg
}

// close terminates the child and reaps it. Closing stdin is the polite
// exit — git leaves its read loop at EOF — and the kill is the backstop
// for a child that ignores it. It is idempotent, and the caller must
// hold the owning source's lock.
func (b *gitBatch) close() {
	if b.closed {
		return
	}
	b.closed = true
	_ = b.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = b.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(batchShutdownGrace):
		_ = b.cmd.Process.Kill()
		<-done
	}
}

// syncBuffer is a bounded, mutex-guarded sink for a child's stderr. The
// child writes to it from the copying goroutine os/exec starts while
// the reader may be inspecting it, so the lock is what keeps that from
// being a data race.
type syncBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

// Write appends to the buffer, dropping whatever exceeds the cap. It
// always reports the full length as written so the child never sees a
// short write on its stderr.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if room := s.limit - s.buf.Len(); room > 0 {
		if len(p) > room {
			s.buf.Write(p[:room])
		} else {
			s.buf.Write(p)
		}
	}
	return len(p), nil
}

// String returns what was captured so far.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
