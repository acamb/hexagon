package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
	"github.com/coder/websocket"
)

const (
	// terminalPingInterval keeps idle connections alive through proxies and
	// notices a peer that went away without closing.
	terminalPingInterval = 30 * time.Second
	// terminalReadLimit bounds a single message from the browser. Keystrokes
	// are tiny; a large paste is the case that needs the headroom.
	terminalReadLimit = 1 << 20
	// terminalBufferSize is how much container output is forwarded per frame.
	terminalBufferSize = 32 << 10
)

// The command a terminal attaches with is `tmux new-session -A -D`: -A creates
// the session if it is not there and attaches otherwise, so reconnecting after a
// reload is the same operation as connecting the first time. -D detaches any
// client that is already attached.
//
// -D is what keeps the container tidy. Docker does not kill an exec'd process
// when its connection closes, so every closed tab would otherwise leave a tmux
// client attached for ever — and since tmux sizes a window to its smallest
// client, one stale 80x24 client would shrink the terminal for the live one.
// The cost is that a second tab takes the terminal over from the first rather
// than watching alongside it.
//
// The session itself is created by the bootstrap, which owns the name; handlers
// take it from there rather than repeating the string.
const tmuxSessionName = session.TmuxSession

// handleTerminal bridges a browser WebSocket to a tmux session inside the
// container. Closing the socket detaches; tmux, and whatever Claude Code is
// doing inside it, keeps running.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	session, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}
	if session.ContainerID == "" || session.Status != store.SessionStatusRunning {
		writeError(w, http.StatusConflict, "session is not running")
		return
	}

	// A browser always sends Origin on a WebSocket handshake, so here — unlike
	// the rest of the API — a missing one is refused too: this endpoint hands
	// out a shell, and there is no legitimate browser caller without it.
	if r.Header.Get("Origin") == "" || !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.originPatterns(),
	})
	if err != nil {
		s.log.Warn("terminal handshake failed", "session", session.ID, "err", err)
		return
	}
	conn.SetReadLimit(terminalReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	exec, err := s.docker.AttachExec(ctx, dockerx.ExecRequest{
		ContainerID: session.ContainerID,
		Cmd:         []string{"tmux", "new-session", "-A", "-D", "-s", tmuxSessionName, "-c", dockerx.WorkspaceMount},
		Env:         []string{"TERM=xterm-256color"},
		Size:        sizeFromQuery(r.URL.Query()),
	})
	if err != nil {
		s.log.Error("attach terminal", "session", session.ID, "err", err)
		conn.Close(websocket.StatusInternalError, "cannot attach to the session")
		return
	}

	s.log.Info("terminal attached", "session", session.ID, "exec", exec.ID)
	s.pumpTerminal(ctx, conn, exec)
	exec.Close()
	conn.Close(websocket.StatusNormalClosure, "")
	s.log.Info("terminal detached", "session", session.ID, "exec", exec.ID)
}

// pumpTerminal moves bytes in both directions until either side stops.
func (s *Server) pumpTerminal(ctx context.Context, conn *websocket.Conn, exec *dockerx.Exec) {
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	// Container to browser. Terminal output is binary: it is not necessarily
	// valid UTF-8 at frame boundaries, and a text frame would have to be.
	go func() {
		defer stop()
		buf := make([]byte, terminalBufferSize)
		for {
			n, readErr := exec.Output.Read(buf)
			if n > 0 {
				if err := conn.Write(ctx, websocket.MessageBinary, buf[:n]); err != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// Browser to container: binary frames are keystrokes, text frames are
	// control messages.
	go func() {
		defer stop()
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			switch kind {
			case websocket.MessageBinary:
				if _, err := exec.Conn.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				s.applyTerminalControl(ctx, exec, data)
			}
		}
	}()

	go s.keepAlive(ctx, conn, done)

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// keepAlive pings the browser periodically. Ping needs a concurrent reader to
// collect the pong, which the browser-to-container pump provides.
func (s *Server) keepAlive(ctx context.Context, conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(terminalPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, terminalPingInterval)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

type terminalControl struct {
	Type string `json:"type"`
	Cols uint   `json:"cols"`
	Rows uint   `json:"rows"`
}

// applyTerminalControl handles the browser's out-of-band messages. An
// unparseable or unknown one is ignored: a malformed control message is no
// reason to drop a working terminal.
func (s *Server) applyTerminalControl(ctx context.Context, exec *dockerx.Exec, data []byte) {
	var control terminalControl
	if err := json.Unmarshal(data, &control); err != nil {
		s.log.Debug("unparseable terminal control message", "err", err)
		return
	}
	if control.Type != "resize" {
		return
	}
	size := dockerx.TerminalSize{Cols: control.Cols, Rows: control.Rows}
	if err := s.docker.ResizeExec(ctx, exec.ID, size); err != nil {
		s.log.Warn("resize terminal", "exec", exec.ID, "err", err)
	}
}

// sizeFromQuery reads the geometry the browser reports when connecting, so the
// first screen is drawn at the right size instead of at 80x24 and then redrawn.
func sizeFromQuery(query url.Values) dockerx.TerminalSize {
	parse := func(name string) uint {
		n, err := strconv.ParseUint(query.Get(name), 10, 32)
		if err != nil {
			return 0
		}
		return uint(n)
	}
	return dockerx.TerminalSize{Cols: parse("cols"), Rows: parse("rows")}
}

// originPatterns is the allowed WebSocket origin, as the library expects it:
// host only.
func (s *Server) originPatterns() []string {
	public, err := url.Parse(s.cfg.PublicURL)
	if err != nil || public.Host == "" {
		return nil
	}
	return []string{public.Host}
}

// sessionOr404 loads the session named in the path, scoped to the caller.
func (s *Server) sessionOr404(w http.ResponseWriter, r *http.Request) (*store.Session, bool) {
	session, err := s.store.SessionByID(r.Context(), s.user(r).ID, r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such session")
		return nil, false
	case err != nil:
		s.log.Error("load session", "id", r.PathValue("id"), "err", err)
		writeError(w, http.StatusInternalServerError, "cannot load session")
		return nil, false
	}
	return session, true
}
