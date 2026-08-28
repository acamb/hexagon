package dockerx

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestTarDockerfile(t *testing.T) {
	const dockerfile = "FROM node:22-bookworm-slim\nRUN true\n"

	r, err := tarDockerfile(dockerfile)
	if err != nil {
		t.Fatalf("tarDockerfile: %v", err)
	}

	tr := tar.NewReader(r)
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("read first entry: %v", err)
	}
	if header.Name != "Dockerfile" {
		t.Errorf("entry name = %q, want Dockerfile", header.Name)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if string(body) != dockerfile {
		t.Errorf("entry body = %q, want the Dockerfile", body)
	}
	if header.Size != int64(len(dockerfile)) {
		t.Errorf("entry size = %d, want %d", header.Size, len(dockerfile))
	}

	if _, err := tr.Next(); err != io.EOF {
		t.Error("build context holds more than the Dockerfile")
	}
}

func TestDecodeProgressWritesTheStream(t *testing.T) {
	in := strings.NewReader(`{"stream":"Step 1/2 : FROM busybox\n"}
{"stream":"Successfully built abc123\n"}
`)
	var out bytes.Buffer
	if err := decodeProgress(in, &out); err != nil {
		t.Fatalf("decodeProgress: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Step 1/2") || !strings.Contains(got, "Successfully built") {
		t.Errorf("output = %q, want both stream lines", got)
	}
}

// A build that fails reports it inside the stream, with HTTP 200 on the
// request that started it, so this is the only place the failure surfaces.
func TestDecodeProgressReportsAnErrorInTheStream(t *testing.T) {
	in := strings.NewReader(`{"stream":"Step 1/2 : FROM busybox\n"}
{"errorDetail":{"code":1,"message":"The command '/bin/sh -c false' returned a non-zero code: 1"},"error":"The command '/bin/sh -c false' returned a non-zero code: 1"}
`)
	var out bytes.Buffer
	err := decodeProgress(in, &out)
	if err == nil {
		t.Fatal("decodeProgress accepted a failed build")
	}
	if !strings.Contains(err.Error(), "non-zero code") {
		t.Errorf("error = %v, want the daemon message", err)
	}
}
