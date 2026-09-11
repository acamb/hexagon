package dockerx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

// newTestClient points a Client at a fake daemon, so a test can see exactly
// what request a call produced without a real Docker socket. The API version
// is fixed rather than negotiated: negotiation itself calls /version, which
// the fake would otherwise have to answer too.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	host := "tcp://" + strings.TrimPrefix(server.URL, "http://")
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithVersion("1.44"))
	if err != nil {
		t.Fatalf("build test client: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return &Client{cli: cli}
}

func TestPruneContainersCarriesOnlyTheManagedLabelFilter(t *testing.T) {
	var gotQuery url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		json.NewEncoder(w).Encode(map[string]any{"ContainersDeleted": []string{"a"}, "SpaceReclaimed": 42})
	})

	pruned, err := c.PruneContainers(context.Background())
	if err != nil {
		t.Fatalf("PruneContainers: %v", err)
	}
	if pruned.Removed != 1 || pruned.Reclaimed != 42 {
		t.Errorf("Pruned = %+v, want {Removed:1 Reclaimed:42}", pruned)
	}

	var filters map[string]map[string]bool
	if err := json.Unmarshal([]byte(gotQuery.Get("filters")), &filters); err != nil {
		t.Fatalf("decode filters query: %v", err)
	}
	if len(filters) != 1 {
		t.Fatalf("filters = %v, want exactly one key", filters)
	}
	labelNeq, ok := filters["label!"]
	if !ok {
		t.Fatalf("filters = %v, want a label! key, not label", filters)
	}
	if len(labelNeq) != 1 || !labelNeq[LabelManaged+"=true"] {
		t.Errorf("label! filter = %v, want exactly {%q: true}", labelNeq, LabelManaged+"=true")
	}
}

func TestListImagesReportsContainerCounts(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "sha256:aaa", "RepoTags": []string{"node:22"}, "Size": 100, "Created": 1000, "Containers": 2},
			{"Id": "sha256:bbb", "RepoTags": []string{}, "Size": 50, "Created": 2000, "Containers": 0},
		})
	})

	images, err := c.ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("len(images) = %d, want 2", len(images))
	}
	if images[0].Containers != 2 {
		t.Errorf("images[0].Containers = %d, want 2: without it every image looks unused", images[0].Containers)
	}
	if images[1].Containers != 0 {
		t.Errorf("images[1].Containers = %d, want 0", images[1].Containers)
	}
}
