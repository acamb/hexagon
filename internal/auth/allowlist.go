package auth

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/andrea/hexagon/internal/store"
)

// UserLookup is how the allowlist finds out whether a login is already spoken
// for by a different GitHub account.
type UserLookup interface {
	UserByGitHubLogin(ctx context.Context, login string) (*store.User, error)
}

// Allowlist answers one question: may this person use this machine. It is its
// own type rather than a detail of OAuth because that question is not asked
// only at the login — every request asks it, and a login that was admitted last
// week is not evidence about this one.
//
// An entry is a GitHub account id when it parses as an integer, and a login
// otherwise. Ids are the form to prefer: GitHub releases a login when an
// account is renamed, and anyone may then claim it.
type Allowlist struct {
	logins map[string]bool
	ids    map[int64]bool
	users  UserLookup
	log    *slog.Logger
}

// NewAllowlist builds the list from the configured entries.
//
// It fails closed. Without an allowlist any GitHub account on the planet could
// sign in and get a shell on this machine, so an empty list is a startup error
// rather than a permissive default.
func NewAllowlist(entries []string, users UserLookup, log *slog.Logger) (*Allowlist, error) {
	list := &Allowlist{
		logins: map[string]bool{},
		ids:    map[int64]bool{},
		users:  users,
		log:    log,
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if id, err := strconv.ParseInt(entry, 10, 64); err == nil {
			list.ids[id] = true
			continue
		}
		list.logins[strings.ToLower(entry)] = true
	}
	if len(list.logins)+len(list.ids) == 0 {
		return nil, errors.New("HEXAGON_ALLOWED_USERS is required: list the GitHub account ids, or logins, allowed to sign in")
	}
	return list, nil
}

// Allowed reports whether a GitHub account may use Hexagon. It takes the whole
// identity, because a login on its own does not identify anyone for long.
//
// An id match is the end of it: account ids are immutable and cannot be
// claimed. A login match is checked against what this instance has already
// seen — if another account has been known under that login, the login was
// released and re-registered, and this is somebody else. Refusing here turns
// what used to be an accidental unique-constraint failure at insert time, which
// surfaced as an illegible server_error, into a deliberate refusal with a
// message worth reading.
//
// An error means the check could not be made; the caller must treat that as a
// refusal, which is why the bool is false alongside it.
func (a *Allowlist) Allowed(ctx context.Context, login string, githubID int64) (bool, error) {
	if a.ids[githubID] {
		return true, nil
	}
	if !a.logins[strings.ToLower(login)] {
		return false, nil
	}

	known, err := a.users.UserByGitHubLogin(ctx, login)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Nobody has used this login here yet, so there is nothing to conflict
		// with: the allowlist means what it says.
		return true, nil
	case err != nil:
		return false, err
	case known.GitHubID != githubID:
		a.log.Warn("login refused: the allowlisted login now belongs to a different account",
			"login", login, "known_github_id", known.GitHubID, "github_id", githubID)
		return false, nil
	}
	return true, nil
}
