package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Patch is the set of settings that can be written back into the configuration
// file from the running server. A nil field is one the caller is not changing,
// which is not the same as one it is clearing.
type Patch struct {
	GitHubClientID     *string
	GitHubClientSecret *string
	AllowedUsers       *[]string
}

// Update rewrites the configuration file at path with the settings in p and
// leaves everything else in it exactly as it found it.
//
// The edit is made on the decoded JSON document rather than on the file struct,
// so a file keeps the keys it had and gains no others: one that sets three
// things goes on setting three things instead of growing every key Hexagon
// knows at its default. Nothing can be lost that way — loadFile rejects unknown
// keys, so a file that loads holds nothing this package does not understand —
// but the keys come back in alphabetical order, which is the one visible cost.
//
// The file is written through a temporary file in the same directory and
// renamed over the target, so an interrupted write cannot leave the server with
// a configuration it will refuse to start from next time.
func Update(path string, p Patch) error {
	if path == "" {
		return errors.New("no configuration file to write: name one with -config or HEXAGON_CONFIG")
	}

	// Refuse to overwrite a file that does not load. Its permissions, its
	// syntax and its keys are checked by the same function that will read it at
	// the next start, so a file this server cannot understand is never
	// rewritten from a form.
	if _, _, err := loadFile(path, false); err != nil {
		return err
	}

	doc, err := readDocument(path)
	if err != nil {
		return err
	}

	github, _ := doc["github"].(map[string]any)
	if github == nil {
		github = map[string]any{}
	}
	if p.GitHubClientID != nil {
		github["clientId"] = *p.GitHubClientID
	}
	if p.GitHubClientSecret != nil {
		github["clientSecret"] = *p.GitHubClientSecret
	}
	if p.AllowedUsers != nil {
		// Never null: the file is read back by people, and an empty list says
		// "nobody" where null says nothing at all.
		users := *p.AllowedUsers
		if users == nil {
			users = []string{}
		}
		github["allowedUsers"] = users
	}
	doc["github"] = github

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return writeFile(path, append(out, '\n'))
}

// readDocument decodes the configuration file into a plain JSON document. A
// file that is not there yet is an empty one: this is how the first-time wizard
// creates the first configuration file on a machine.
//
// UseNumber keeps every number exactly as it was written, so a value this
// package never looks at cannot be reshaped by being read and written back.
func readDocument(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return map[string]any{}, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	doc := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc, nil
}

// writeFile replaces path atomically, with the permissions the loader demands
// of it.
func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// CreateTemp opens with 0600, which is what the file has to end up with:
	// it holds the OAuth client secret, and it must never exist with wider
	// permissions, even for the moment between writing it and renaming it.
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// Writable reports whether Update could write the file at path.
//
// It answers by trying, because on a POSIX system nothing else is conclusive:
// the mode bits say little about a directory reached through a read-only mount,
// or owned by another user, or covered by an ACL. Nothing survives the check —
// an existing file is opened, not truncated, and a missing one costs a
// temporary file in the directory it would live in.
func Writable(path string) bool {
	if path == "" {
		return false
	}
	if f, err := os.OpenFile(path, os.O_WRONLY, 0o600); err == nil {
		f.Close()
		return true
	} else if !errors.Is(err, os.ErrNotExist) {
		return false
	}

	// The directory may not exist either — on a machine that has never had a
	// configuration file, none of it does — so the probe goes to the nearest
	// ancestor that is there. Creating the missing part is Update's job, not a
	// side effect of being asked a question.
	dir := nearestExisting(filepath.Dir(path))
	if dir == "" {
		return false
	}
	tmp, err := os.CreateTemp(dir, ".config-probe-*")
	if err != nil {
		return false
	}
	tmp.Close()
	os.Remove(tmp.Name())
	return true
}

// nearestExisting walks up from dir to the first directory that exists, and
// returns "" when it runs out of parents without finding one.
func nearestExisting(dir string) string {
	for {
		switch info, err := os.Stat(dir); {
		case err == nil && info.IsDir():
			return dir
		case err == nil:
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
