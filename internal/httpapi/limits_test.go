package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A session is a container, a clone and a workspace directory, so there is a
// number of them past which one user has taken the machine.
func TestCreateSessionRefusesPastTheSessionCap(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	// One short of the cap, inserted rather than provisioned: what is being
	// tested is the count, and twenty real sessions would only be slower.
	for i := range env.cfg.MaxSessionsPerUser - 1 {
		_, err := env.store.DB().Exec(`
			INSERT INTO sessions (id, user_id, title, repo_full_name, repo_clone_url, branch, image_id,
				image_ref, workspace_dir, repo_dir, container_id, status, error, vscode, created_at, updated_at)
			VALUES (?, ?, 'filler', '', '', '', ?, ?, '/w', '', '', 'stopped', '', 0,
				'2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`,
			fmt.Sprintf("filler-%d", i), env.userID(), image.ID, image.ImageRef)
		if err != nil {
			t.Fatalf("insert session %d: %v", i, err)
		}
	}

	body := fmt.Sprintf(`{"imageId":%q}`, image.ID)
	if got := env.postJSON("/api/sessions", body).StatusCode; got != http.StatusAccepted {
		t.Fatalf("the session at the cap = %d, want 202", got)
	}

	resp := env.postJSON("/api/sessions", body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the session past the cap = %d, want 429", resp.StatusCode)
	}
	// The message says what the limit is, so the answer is actionable without
	// reading the configuration.
	if body := env.bodyString(resp); !strings.Contains(body, fmt.Sprint(env.cfg.MaxSessionsPerUser)) {
		t.Errorf("message = %q, want it to name the limit", body)
	}
}

func TestCreateImageRefusesPastTheBuildCap(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	for i := range env.cfg.MaxConcurrentBuilds {
		_, err := env.store.DB().Exec(`
			INSERT INTO images (id, user_id, name, source_type, dockerfile, registry_ref, image_ref,
				status, build_log, error, created_at)
			VALUES (?, ?, ?, 'dockerfile', 'FROM busybox', '', '', 'building', '', '',
				'2026-01-01 00:00:00.000')`,
			fmt.Sprintf("building-%d", i), env.userID(), fmt.Sprintf("building-%d", i))
		if err != nil {
			t.Fatalf("insert image %d: %v", i, err)
		}
	}

	resp := env.postJSON("/api/images", `{"name":"one-more","sourceType":"dockerfile","dockerfile":"FROM busybox"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a build past the cap = %d, want 429", resp.StatusCode)
	}
}

// The health check pings the database and the Docker daemon, which is real work
// for a route anyone who reaches the port can call, and its answer describes the
// machine.
func TestHealthTellsAnUnauthenticatedCallerNothing(t *testing.T) {
	env := newTestEnv(t, "alice")

	public := env.bodyString(env.do(http.MethodGet, "/api/health", nil))
	if strings.Contains(public, "docker") || strings.Contains(public, "uptime") {
		t.Errorf("public health body = %s, want the status alone", public)
	}

	env.signIn()
	authenticated := env.bodyString(env.do(http.MethodGet, "/api/health", nil))
	if !strings.Contains(authenticated, "docker") || !strings.Contains(authenticated, "uptime") {
		t.Errorf("authenticated health body = %s, want the details", authenticated)
	}
}

// The limit is per address, and the address comes from X-Forwarded-For only
// because the test client is the loopback peer a proxy would be.
func TestPublicRoutesAreRateLimitedPerAddress(t *testing.T) {
	env := newTestEnv(t, "alice")

	limited := false
	for range env.cfg.PublicRatePerMinute + 1 {
		if env.do(http.MethodGet, "/api/health", map[string]string{
			"X-Forwarded-For": "203.0.113.1",
		}).StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("one address was never refused in %d requests", env.cfg.PublicRatePerMinute+1)
	}

	// Somebody else's bucket is their own.
	if got := env.do(http.MethodGet, "/api/health", map[string]string{
		"X-Forwarded-For": "203.0.113.2",
	}).StatusCode; got != http.StatusOK {
		t.Errorf("a second address = %d, want 200", got)
	}
}

// A header anyone can set must not choose the bucket unless the peer that sent
// it is the proxy this server runs behind.
func TestClientIPTrustsForwardedForOnlyFromLoopback(t *testing.T) {
	for _, c := range []struct {
		name      string
		remote    string
		forwarded string
		want      string
	}{
		{"no header", "127.0.0.1:5555", "", "127.0.0.1"},
		{"from the proxy", "127.0.0.1:5555", "203.0.113.1", "203.0.113.1"},
		{"a chain the proxy appended to", "127.0.0.1:5555", "10.0.0.9, 203.0.113.1", "203.0.113.1"},
		{"from anywhere else", "198.51.100.7:5555", "203.0.113.1", "198.51.100.7"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "/api/health", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			r.RemoteAddr = c.remote
			if c.forwarded != "" {
				r.Header.Set("X-Forwarded-For", c.forwarded)
			}
			if got := clientIP(r); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}
