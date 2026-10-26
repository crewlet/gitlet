package gitx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// readerImpl drives one git cat-file --batch process. A mutex serializes
// requests, so a Reader may be shared between goroutines even though one
// per operation is the intended use.
type readerImpl struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *limitedBuffer
	cancel context.CancelFunc
	exited func() // from supervise
	err    error  // set once the process can no longer be used
	closed bool
}

// openReader starts the process. It lives until Close or until ctx is done.
func (r *Repo) openReader(ctx context.Context) (*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("gitx: cat-file: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir = r.Root
	cmd.Env = r.environ(nil)
	exited := supervise(cmd)
	stderr := &limitedBuffer{max: 64 << 10}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gitx: cat-file: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gitx: cat-file: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("gitx: cat-file: %w", err)
	}
	rd := &Reader{}
	rd.cmd, rd.stdin, rd.stdout, rd.stderr = cmd, stdin, bufio.NewReaderSize(stdout, 64<<10), stderr
	rd.cancel, rd.exited = cancel, exited
	return rd, nil
}

func (rd *readerImpl) readFile(rev, path string) ([]byte, error) {
	if err := checkRev(rev); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("gitx: invalid path %q", path)
	}
	spec := rev + ":" + path
	data, typ, _, _, err := rd.read(spec, 0)
	if err != nil {
		return nil, err
	}
	if typ != "blob" {
		return nil, fmt.Errorf("gitx: %s is a %s, not a file", spec, typ)
	}
	return data, nil
}

func (rd *readerImpl) readObject(spec string, max int64) ([]byte, int64, bool, error) {
	data, _, size, ok, err := rd.read(spec, max)
	return data, size, ok, err
}

// read asks for one object. Objects over max bytes (max > 0) are skipped
// on the pipe and reported with ok false.
func (rd *readerImpl) read(spec string, max int64) (data []byte, typ string, size int64, ok bool, err error) {
	if spec == "" || strings.ContainsAny(spec, "\x00\n\r") {
		return nil, "", 0, false, fmt.Errorf("gitx: invalid object name %q", spec)
	}
	rd.mu.Lock()
	defer rd.mu.Unlock()
	switch {
	case rd.closed:
		return nil, "", 0, false, errors.New("gitx: cat-file: reader is closed")
	case rd.cmd == nil:
		return nil, "", 0, false, errors.New("gitx: cat-file: reader is not open")
	case rd.err != nil:
		return nil, "", 0, false, rd.err
	}

	if _, err := io.WriteString(rd.stdin, spec+"\n"); err != nil {
		return nil, "", 0, false, rd.fail(err)
	}
	header, err := rd.stdout.ReadString('\n')
	if err != nil {
		return nil, "", 0, false, rd.fail(err)
	}
	header = strings.TrimSuffix(header, "\n")
	if strings.HasSuffix(header, " missing") || strings.HasSuffix(header, " ambiguous") {
		return nil, "", 0, false, notFound(spec)
	}
	fields := strings.Fields(header) // <oid> <type> <size>
	if len(fields) != 3 {
		return nil, "", 0, false, rd.fail(fmt.Errorf("unexpected header %q", header))
	}
	typ = fields[1]
	size, err = strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 || size >= math.MaxInt {
		return nil, "", 0, false, rd.fail(fmt.Errorf("unexpected header %q", header))
	}
	if max > 0 && size > max {
		if _, err := io.CopyN(io.Discard, rd.stdout, size+1); err != nil {
			return nil, "", 0, false, rd.fail(err)
		}
		return nil, typ, size, false, nil
	}
	buf := make([]byte, size+1) // content and the newline after it
	if _, err := io.ReadFull(rd.stdout, buf); err != nil {
		return nil, "", 0, false, rd.fail(err)
	}
	if buf[size] != '\n' {
		return nil, "", 0, false, rd.fail(errors.New("object not followed by a newline"))
	}
	return buf[:size], typ, size, true, nil
}

// fail marks the reader unusable, since the pipe may be out of step with
// the requests, and stops the process.
func (rd *readerImpl) fail(err error) error {
	msg := strings.TrimSpace(rd.stderr.String())
	if msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	rd.err = fmt.Errorf("gitx: cat-file --batch: %w", err)
	rd.cancel()
	return rd.err
}

func (rd *readerImpl) close() error {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.closed || rd.cmd == nil {
		rd.closed = true
		return nil
	}
	rd.closed = true
	rd.stdin.Close() // cat-file exits at end of input
	done := make(chan error, 1)
	go func() { done <- rd.cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(waitDelay):
		rd.cancel()
		err = <-done
	}
	rd.cancel()
	rd.exited()
	// Only a failure git reported itself is worth returning: a process
	// stopped by a failed read or by its context was already accounted for.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 && rd.err == nil {
		return fmt.Errorf("gitx: cat-file --batch: %w: %s", err, strings.TrimSpace(rd.stderr.String()))
	}
	return nil
}
