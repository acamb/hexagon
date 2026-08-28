package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// pagedRepos serves a two-page listing the way GitHub does, with the next page
// announced only through the Link header.
func pagedRepos(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/user/repos" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"full_name":"acme/second","clone_url":"https://github.com/acme/second.git","default_branch":"trunk","private":true,"updated_at":"2026-02-02T10:00:00Z"}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?page=2>; rel="next", <%s/user/repos?page=2>; rel="last"`, server.URL, server.URL))
		fmt.Fprint(w, `[{"full_name":"acme/first","clone_url":"https://github.com/acme/first.git","default_branch":"main","description":"the first one","updated_at":"2026-03-03T10:00:00Z"}]`)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestListReposFollowsPagination(t *testing.T) {
	var requests atomic.Int32
	server := pagedRepos(t, &requests)

	repos, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), "token")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repositories, want both pages", len(repos))
	}
	if repos[0].FullName != "acme/first" || repos[1].FullName != "acme/second" {
		t.Errorf("repositories = %+v", repos)
	}
	if repos[0].Description != "the first one" || repos[0].DefaultBranch != "main" {
		t.Errorf("first repository = %+v", repos[0])
	}
	if !repos[1].Private || repos[1].DefaultBranch != "trunk" {
		t.Errorf("second repository = %+v", repos[1])
	}
	if repos[0].UpdatedAt.IsZero() {
		t.Error("updated_at did not parse")
	}
}

func TestNextPageURL(t *testing.T) {
	cases := map[string]string{
		`<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=9>; rel="last"`:  "https://api.github.com/user/repos?page=2",
		`<https://api.github.com/user/repos?page=1>; rel="prev", <https://api.github.com/user/repos?page=1>; rel="first"`: "",
		"":        "",
		"garbage": "",
	}
	for header, want := range cases {
		if got := nextPageURL(header); got != want {
			t.Errorf("nextPageURL(%q) = %q, want %q", header, got, want)
		}
	}
}

// A token GitHub refuses is not a transient failure, and the caller has to be
// able to tell it apart so it can ask for a new sign-in.
func TestListReposReportsARejectedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	}))
	defer server.Close()

	_, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), "stale")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}
}

func TestRepoCacheServesRepeatCallsWithoutRefetching(t *testing.T) {
	var requests atomic.Int32
	server := pagedRepos(t, &requests)
	cache := NewRepoCache(NewWithBaseURL(server.URL), time.Minute)

	ctx := context.Background()
	if _, err := cache.List(ctx, "user-1", "token"); err != nil {
		t.Fatalf("first List: %v", err)
	}
	firstRound := requests.Load()
	if firstRound != 2 {
		t.Fatalf("first listing made %d requests, want 2 (one per page)", firstRound)
	}

	if _, err := cache.List(ctx, "user-1", "token"); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if requests.Load() != firstRound {
		t.Errorf("a cached listing still hit the API: %d requests", requests.Load())
	}

	// Another user has their own repositories, so their listing is separate.
	if _, err := cache.List(ctx, "user-2", "other-token"); err != nil {
		t.Fatalf("List for another user: %v", err)
	}
	if requests.Load() != firstRound*2 {
		t.Errorf("another user's listing was served from the first user's cache")
	}

	cache.Invalidate("user-1")
	if _, err := cache.List(ctx, "user-1", "token"); err != nil {
		t.Fatalf("List after Invalidate: %v", err)
	}
	if requests.Load() != firstRound*3 {
		t.Errorf("Invalidate did not force a refetch: %d requests", requests.Load())
	}
}

func TestRepoCacheExpires(t *testing.T) {
	var requests atomic.Int32
	server := pagedRepos(t, &requests)
	cache := NewRepoCache(NewWithBaseURL(server.URL), time.Nanosecond)

	ctx := context.Background()
	if _, err := cache.List(ctx, "user-1", "token"); err != nil {
		t.Fatalf("first List: %v", err)
	}
	if _, err := cache.List(ctx, "user-1", "token"); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if requests.Load() != 4 {
		t.Errorf("made %d requests, want 4: an expired entry must be refetched", requests.Load())
	}
}
