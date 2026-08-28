package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCurrentUser(t *testing.T) {
	var gotAuth, gotVersion string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login":"alice","id":42,"avatar_url":"https://example.test/a.png"}`))
	}))
	defer stub.Close()

	user, err := NewWithBaseURL(stub.URL).CurrentUser(context.Background(), "gho_token")
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if user.Login != "alice" || user.ID != 42 {
		t.Errorf("user = %+v", user)
	}
	if gotAuth != "Bearer gho_token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotVersion != apiVersion {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", gotVersion, apiVersion)
	}
}

func TestCurrentUserSurfacesAPIErrors(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer stub.Close()

	_, err := NewWithBaseURL(stub.URL).CurrentUser(context.Background(), "stale")
	if err == nil {
		t.Fatal("CurrentUser accepted a 401")
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error = %v, want it to carry the GitHub message", err)
	}
}
