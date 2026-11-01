package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

type credentialFD struct {
	ctx context.Context
	fd  int
}

func (r credentialFD) Read(b []byte) (int, error) {
	for {
		if r.ctx.Err() != nil {
			return 0, r.ctx.Err()
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		n, e := unix.Poll(fds, 50)
		if e == unix.EINTR {
			continue
		}
		if e != nil {
			return 0, e
		}
		if n == 0 {
			continue
		}
		n, e = unix.Read(r.fd, b)
		if n == 0 && e == nil {
			return 0, io.EOF
		}
		return n, e
	}
}

type terminalOps struct {
	get func(int) (*unix.Termios, error)
	set func(int, *unix.Termios) error
}

func readCredentialFD(ctx context.Context, f *os.File, w io.Writer, stdin bool, ops terminalOps) (value []byte, err error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	fd := int(f.Fd())
	state, e := ops.get(fd)
	terminal := e == nil
	if !terminal && !stdin {
		return nil, credentialError(ctx, "use --password-stdin for nonterminal input")
	}
	if terminal {
		changed := *state
		changed.Lflag &^= unix.ECHO | unix.ECHONL
		defer func() {
			if ops.set(fd, state) != nil {
				clear(value)
				value = nil
				err = credentialError(ctx, "cannot restore terminal state")
			}
		}()
		if ops.set(fd, &changed) != nil {
			return nil, credentialError(ctx, "cannot disable terminal echo")
		}
		prompt := "App password (input hidden): "
		n, e := io.WriteString(w, prompt)
		if e != nil || n != len(prompt) {
			return nil, credentialError(ctx, "prompt write failed")
		}
	}
	return credentialLine(ctx, credentialFD{ctx, fd}, terminal)
}
func readCredentialInput(ctx context.Context, r io.Reader, w io.Writer, stdin bool) ([]byte, error) {
	f, ok := r.(*os.File)
	if !ok {
		return nil, credentialError(ctx, "stdin file descriptor required")
	}
	return readCredentialFD(ctx, f, w, stdin, terminalOps{func(fd int) (*unix.Termios, error) { return unix.IoctlGetTermios(fd, unix.TIOCGETA) }, func(fd int, v *unix.Termios) error { return unix.IoctlSetTermios(fd, unix.TIOCSETA, v) }})
}
