package httpapi

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/andrea/hexagon/internal/session"
)

// handleVSCode reverse-proxies to the code-server running inside a session's
// container. It carries a whole other application's traffic — code-server's
// own HTTP and its own WebSocket — which is why the routes above match every
// method: there is no verb this handler can rule out in advance.
func (s *Server) handleVSCode(w http.ResponseWriter, r *http.Request) {
	found, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}

	// code-server emits relative URLs and needs a directory as its base; a
	// request without the trailing slash would resolve them one segment too
	// high.
	if r.URL.Path == "/api/sessions/"+found.ID+"/vscode" {
		redirect := *r.URL
		redirect.Path += "/"
		http.Redirect(w, r, redirect.String(), http.StatusFound)
		return
	}

	base, err := s.sessions.VSCodeEndpoint(r.Context(), found)
	switch {
	case errors.Is(err, session.ErrVSCodeDisabled), errors.Is(err, session.ErrVSCodeNotReady):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, session.ErrNoContainer):
		writeError(w, http.StatusConflict, "this session has no container yet")
		return
	case err != nil:
		s.log.Error("vscode endpoint", "session", found.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot reach this session's editor")
		return
	}
	target, err := url.Parse(base)
	if err != nil {
		s.log.Error("parse vscode endpoint", "session", found.ID, "endpoint", base, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot reach this session's editor")
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// code-server is served at the root of its own port; the prefix that
			// names the session is Hexagon's, and it strips it the way the reverse
			// proxy recipe in code-server's documentation does.
			pr.Out.URL.Path = "/" + pr.In.PathValue("path")
			pr.Out.URL.RawPath = ""
			pr.SetXForwarded()
			// code-server runs with --auth none and has no use for either header.
			// The session cookie must not reach the container: it is the
			// credential that authorises creating sessions and attaching
			// terminals, and that container runs an agent over repository
			// content nobody here has read. ReverseProxy strips hop-by-hop
			// headers and forwards the rest, Cookie included.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("vscode proxy", "session", found.ID, "err", err)
			http.Error(w, "VS Code is not answering in this session yet. Reload in a moment.",
				http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
