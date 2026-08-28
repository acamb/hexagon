package dockerx

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/docker/docker/api/types/container"
)

// Terminal geometry limits. The size arrives from the browser, so it is clamped
// rather than trusted, and a missing size falls back to the classic default.
const (
	defaultCols = 80
	defaultRows = 24
	maxCols     = 1000
	maxRows     = 500
)

// TerminalSize is a terminal geometry in character cells.
type TerminalSize struct {
	Cols uint
	Rows uint
}

func (s TerminalSize) normalized() TerminalSize {
	if s.Cols == 0 {
		s.Cols = defaultCols
	}
	if s.Rows == 0 {
		s.Rows = defaultRows
	}
	return TerminalSize{Cols: min(s.Cols, maxCols), Rows: min(s.Rows, maxRows)}
}

// ExecRequest describes an interactive command to run inside a container.
type ExecRequest struct {
	ContainerID string
	Cmd         []string
	Env         []string
	WorkingDir  string
	User        string
	Size        TerminalSize
}

// Exec is a command attached to a container's TTY. Because the exec runs with a
// TTY there is no stream multiplexing: Output carries the terminal bytes as they
// are, and writing to Conn is typing.
type Exec struct {
	ID     string
	Conn   net.Conn
	Output io.Reader
	// Closer releases the attached streams.
	Closer io.Closer
}

// Close detaches. The command itself keeps running in the container, which is
// the point: closing a terminal must not kill the tmux session behind it.
func (e *Exec) Close() error {
	if e.Closer == nil {
		return nil
	}
	return e.Closer.Close()
}

// closerFunc adapts a plain detach function to io.Closer.
type closerFunc func()

func (f closerFunc) Close() error {
	f()
	return nil
}

// AttachExec starts a command with a TTY and hands back the attached streams.
func (c *Client) AttachExec(ctx context.Context, req ExecRequest) (*Exec, error) {
	size := req.Size.normalized()

	created, err := c.cli.ContainerExecCreate(ctx, req.ContainerID, container.ExecOptions{
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		ConsoleSize:  &[2]uint{size.Rows, size.Cols},
		Env:          req.Env,
		Cmd:          req.Cmd,
		WorkingDir:   req.WorkingDir,
		User:         req.User,
	})
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}

	attached, err := c.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("attach exec: %w", err)
	}

	return &Exec{
		ID:     created.ID,
		Conn:   attached.Conn,
		Output: attached.Reader,
		Closer: closerFunc(attached.Close),
	}, nil
}

// ResizeExec tells the container's TTY how large the browser's terminal is.
func (c *Client) ResizeExec(ctx context.Context, execID string, size TerminalSize) error {
	s := size.normalized()
	err := c.cli.ContainerExecResize(ctx, execID, container.ResizeOptions{Height: s.Rows, Width: s.Cols})
	if err != nil {
		return fmt.Errorf("resize exec: %w", err)
	}
	return nil
}
