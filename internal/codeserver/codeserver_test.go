package codeserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// releaseTarball builds a minimal code-server release: a tarball whose sole
// content is bin/code-server, wrapped in the top-level directory the real
// release ships so --strip-components=1 has something to strip.
func releaseTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	body := []byte("#!/bin/sh\necho code-server\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "code-server-4.135.0-linux-amd64/bin/code-server",
		Mode: 0o755,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func TestEnsureDownloadsAndExtractsOnce(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is not on PATH")
	}

	tarball := releaseTarball(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write(tarball)
	}))
	defer server.Close()

	dir := filepath.Join(t.TempDir(), "code-server")
	src := New(Config{Dir: dir, Version: "4.135.0", BaseURL: server.URL})

	got, err := src.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got != dir {
		t.Errorf("Ensure returned %q, want %q", got, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "code-server")); err != nil {
		t.Errorf("the release was not extracted: %v", err)
	}
	if requests != 1 {
		t.Fatalf("made %d requests, want 1", requests)
	}

	// An install that is already there is never touched again: no second
	// request, and calling Ensure again just reports the same directory.
	got, err = src.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	if got != dir {
		t.Errorf("Ensure again returned %q, want %q", got, dir)
	}
	if requests != 1 {
		t.Errorf("made %d requests on a second Ensure, want still 1", requests)
	}
}

func TestEnsureRejectsANonOKResponse(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is not on PATH")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	dir := filepath.Join(t.TempDir(), "code-server")
	src := New(Config{Dir: dir, Version: "4.135.0", BaseURL: server.URL})

	if _, err := src.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure did not report the failed download")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a failed download left a directory an Ensure would accept")
	}
}
