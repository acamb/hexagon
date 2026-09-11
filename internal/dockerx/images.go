package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/jsonmessage"
)

// ImageSummary is one image as the daemon reports it: not a Hexagon row, but
// what "docker images" would show. Containers is how many containers, running
// or not, are based on it — without that count every image looks unused.
type ImageSummary struct {
	ID         string
	Tags       []string
	Size       int64
	Created    time.Time
	Containers int64
}

// BuildImage builds a single-file build context containing dockerfile and tags
// the result. Build output is streamed to logs as it arrives.
func (c *Client) BuildImage(ctx context.Context, dockerfile, tag string, logs io.Writer) error {
	buildContext, err := tarDockerfile(dockerfile)
	if err != nil {
		return err
	}

	resp, err := c.cli.ImageBuild(ctx, buildContext, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		// Drop intermediate containers, including on failure: a failed build
		// should not leave anything behind for the user to clean up.
		Remove:      true,
		ForceRemove: true,
		// Refresh the base image, so rebuilding an image is a way to pick up
		// upstream security updates.
		PullParent: true,
	})
	if err != nil {
		return fmt.Errorf("start build: %w", err)
	}
	defer resp.Body.Close()

	return decodeProgress(resp.Body, logs)
}

// InspectImage returns the content-addressable id of the local image ref
// currently resolves to. Unlike ref itself, which for an image Hexagon built is
// a tag that a later rebuild reassigns, this id names the exact content — it is
// how a session pins the image it was created with even after that tag moves.
func (c *Client) InspectImage(ctx context.Context, ref string) (string, error) {
	inspected, err := c.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect image %s: %w", ref, err)
	}
	return inspected.ID, nil
}

// PullImage fetches ref from its registry. Only public images are supported:
// Hexagon has no registry credentials to offer.
func (c *Client) PullImage(ctx context.Context, ref string, logs io.Writer) error {
	body, err := c.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("start pull: %w", err)
	}
	defer body.Close()

	return decodeProgress(body, logs)
}

// ListImages returns every image the daemon holds, container counts included:
// the Engine API computes those unconditionally, so nothing has to be asked for
// separately.
func (c *Client) ListImages(ctx context.Context) ([]ImageSummary, error) {
	images, err := c.cli.ImageList(ctx, image.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}

	out := make([]ImageSummary, 0, len(images))
	for _, img := range images {
		out = append(out, ImageSummary{
			ID:         img.ID,
			Tags:       img.RepoTags,
			Size:       img.Size,
			Created:    time.Unix(img.Created, 0),
			Containers: img.Containers,
		})
	}
	return out, nil
}

// RemoveImage deletes a local image by reference or id.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.cli.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true})
	if err != nil {
		return fmt.Errorf("remove image %s: %w", ref, err)
	}
	return nil
}

// SaveImage writes ref as a `docker save` tar stream to w, uncompressed: the
// caller decides whether and how to compress it.
func (c *Client) SaveImage(ctx context.Context, ref string, w io.Writer) error {
	body, err := c.cli.ImageSave(ctx, []string{ref})
	if err != nil {
		return fmt.Errorf("save image %s: %w", ref, err)
	}
	defer body.Close()

	if _, err := io.Copy(w, body); err != nil {
		return fmt.Errorf("save image %s: %w", ref, err)
	}
	return nil
}

// LoadImage reads a `docker save` tar stream from r and loads it into the
// daemon, writing progress to logs. A failure during the load is reported
// inside that same progress stream, the way BuildImage's is.
func (c *Client) LoadImage(ctx context.Context, r io.Reader, logs io.Writer) error {
	resp, err := c.cli.ImageLoad(ctx, r)
	if err != nil {
		return fmt.Errorf("start load: %w", err)
	}
	defer resp.Body.Close()

	return decodeProgress(resp.Body, logs)
}

// TagImage adds target as a second name for the image source already resolves
// to.
func (c *Client) TagImage(ctx context.Context, source, target string) error {
	if err := c.cli.ImageTag(ctx, source, target); err != nil {
		return fmt.Errorf("tag image %s as %s: %w", source, target, err)
	}
	return nil
}

// tarDockerfile wraps a Dockerfile in the tar archive the build endpoint
// expects. Hexagon builds have no build context beyond the Dockerfile itself,
// so COPY and ADD of local paths will not work — by design: the image describes
// the tools, the repository arrives as a bind mount.
func tarDockerfile(dockerfile string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	body := []byte(dockerfile)
	err := tw.WriteHeader(&tar.Header{
		Name:    "Dockerfile",
		Mode:    0o600,
		Size:    int64(len(body)),
		ModTime: time.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("write build context header: %w", err)
	}
	if _, err := tw.Write(body); err != nil {
		return nil, fmt.Errorf("write build context: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close build context: %w", err)
	}
	return &buf, nil
}

// decodeProgress turns the daemon's JSON progress stream into plain lines on
// logs. A build that fails reports it inside the stream, not through the HTTP
// status, so the error surfaces here rather than from the call that started it.
func decodeProgress(r io.Reader, logs io.Writer) error {
	err := jsonmessage.DisplayJSONMessagesStream(r, logs, 0, false, nil)
	if err == nil {
		return nil
	}
	var jsonErr *jsonmessage.JSONError
	if errors.As(err, &jsonErr) {
		return fmt.Errorf("%s", jsonErr.Message)
	}
	return err
}
