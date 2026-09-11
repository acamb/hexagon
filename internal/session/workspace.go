package session

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrea/hexagon/internal/store"
)

// WorkspaceInfo answers whether a session has a /workspace directory to
// export, and how big it is.
type WorkspaceInfo struct {
	Exists bool
	Files  int
	Bytes  int64
}

// WorkspaceInfo reports what a session's RepoDir holds on the host, without
// reading a byte of any file in it. It is read on every click of the export
// button rather than cached on the session response: what it says has to be
// true at the moment the user is told it, and the workspace changes while a
// running session's container writes to it.
func (m *Manager) WorkspaceInfo(session *store.Session) (WorkspaceInfo, error) {
	dir, err := m.checkedRepoDir(session)
	if err != nil {
		return WorkspaceInfo{}, err
	}

	switch info, err := os.Stat(dir); {
	case os.IsNotExist(err):
		return WorkspaceInfo{}, nil
	case err != nil:
		return WorkspaceInfo{}, err
	case !info.IsDir():
		return WorkspaceInfo{}, fmt.Errorf("%s is not a directory", dir)
	}

	result := WorkspaceInfo{Exists: true}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		result.Files++
		result.Bytes += info.Size()
		return nil
	})
	if err != nil {
		return WorkspaceInfo{}, err
	}
	return result, nil
}

// WriteWorkspaceArchive streams a gzip-compressed tar of a session's RepoDir
// to w. Nothing is staged: every byte written to w comes straight from a file
// being read, which is what lets the caller answer the request without ever
// keeping the archive on Hexagon's own disk.
//
// It honours ctx, so a browser that cancels the download stops the walk
// instead of reading a whole tree into a socket nobody is listening to.
//
// A failure here is deliberately not turned into a clean early return: the
// caller has already written the response header by the time this can fail
// part way through, and closing the gzip stream anyway would produce an
// archive that looks complete but is missing everything after the failure.
// Leaving the gzip trailer unwritten is what makes `tar -xzf` refuse the file
// outright instead of silently handing over a truncated tree.
func (m *Manager) WriteWorkspaceArchive(ctx context.Context, session *store.Session, w io.Writer) error {
	dir, err := m.checkedRepoDir(session)
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	var skipped int
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case path == dir:
			return nil
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			// Archived as a symlink member, its target never opened: following it
			// would let a link anywhere on the host be read through this endpoint.
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header, err := tar.FileInfoHeader(info, target)
			if err != nil {
				return err
			}
			header.Name = name
			return tw.WriteHeader(header)

		case d.IsDir():
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = name + "/"
			return tw.WriteHeader(header)

		case info.Mode().IsRegular():
			// Opened before the header is written, deliberately: a header this
			// process then cannot fill in would misalign every entry after it in
			// the tar stream, so a file it has no permission to read is skipped
			// exactly as a fifo is, rather than failing the whole export over one
			// file nobody but its owner can open.
			f, err := os.Open(path)
			if err != nil {
				skipped++
				return nil
			}
			defer f.Close()
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = name
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			return err

		default:
			// Sockets, fifos, devices: nothing a restored workspace needs, and
			// opening one could block forever instead of ever returning bytes.
			skipped++
			return nil
		}
	})

	if skipped > 0 {
		m.log.Warn("workspace export skipped entries", "session", session.ID, "skipped", skipped)
	}
	if walkErr != nil {
		return walkErr
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// checkedRepoDir resolves a session's RepoDir and refuses it unless it is
// strictly inside WorkspaceRoot, the same refusal removeWorkspace makes
// before a recursive delete. RepoDir comes out of the database, and this is a
// read of a whole directory tree: a corrupted or hand-edited row must fail
// rather than tar the rest of the disk.
func (m *Manager) checkedRepoDir(session *store.Session) (string, error) {
	return resolveInsideWorkspaceRoot(m.cfg.WorkspaceRoot, session.RepoDir)
}

// resolveInsideWorkspaceRoot resolves dir to an absolute path and refuses it
// unless it is strictly inside root, naming both in the error. root itself is
// refused too, which is the boundary removeWorkspace and checkedRepoDir both
// need: root is where every session's own directory lives, never a session's
// own content.
func resolveInsideWorkspaceRoot(root, dir string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if target == absRoot || !strings.HasPrefix(target, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing to use %s: it is not inside %s", target, absRoot)
	}
	return target, nil
}
