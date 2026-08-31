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
//
// One setting is missing on purpose, and its absence is the whole of the rule:
// a wrong secretKey cannot be corrected from the page that wrote it, because it
// makes every sealed token undecryptable. It stays a file edit and a restart,
// and it is left out of this type rather than checked in a handler, so there is
// no request that can express the change and nothing to silently ignore.
//
// Addr was once missing for a related but weaker reason — a wrong listen
// address is a port nobody can reach, and the settings page is behind that
// port. What makes it survivable here is that the address is read only when the
// process starts: a bad one is saved, reported as waiting for a restart, and
// correctable from the same page right up until that restart. The residual risk
// is real and belongs to the restart, not to the save, which is why the caller
// is expected to refuse anything that is not a host and a port before it gets
// here — see httpapi.checkAddr.
type Patch struct {
	Addr          *string
	PublicURL     *string
	InsecureHTTP  *bool
	DataDir       *string
	WorkspaceRoot *string
	Debug         *bool

	GitHubClientID     *string
	GitHubClientSecret *string
	AllowedUsers       *[]string
	GitHubAPIURL       *string

	BitbucketAPIURL *string

	// ClaudeCredentials sets the file mounted into session containers, where an
	// empty string is itself a value: mount nothing.
	ClaudeCredentials *string
	// ClaudeCredentialsDefault removes the key instead, putting the mount back
	// to ~/.claude/.credentials.json. It is a field of its own because this is
	// the one setting where an empty value is a choice, so "clear it" and "set
	// it to empty" cannot be the same request.
	ClaudeCredentialsDefault bool
	AnthropicAPIKey          *string
	ClaudeBinary             *string
	ClaudeModel              *string

	GitUserName  *string
	GitUserEmail *string

	VSCodeDir     *string
	VSCodeVersion *string

	DockerHost *string
	DockerCLI  *string

	MaxSessionsPerUser  *int
	MaxConcurrentBuilds *int
	PublicRatePerMinute *int
}

// patchField is one setting on its way into the configuration document: where
// it lives, and a typed pointer that is nil when the caller is not changing it.
type patchField struct {
	path  []string
	value any
	// keepEmpty writes an empty value instead of removing the key. Only
	// claude.credentials needs it: everywhere else an empty string, a false and
	// an absent key resolve identically, so writing one would add noise and no
	// meaning.
	keepEmpty bool
}

// fields lays the patch out over the configuration document.
//
// A table rather than an assignment per setting: this reaches twenty keys in
// eight groups, and a list of paths is the only shape of that which stays
// readable as the list grows. The order is the order of the file.
func (p Patch) fields() []patchField {
	return []patchField{
		{path: []string{"addr"}, value: p.Addr},
		{path: []string{"publicUrl"}, value: p.PublicURL},
		{path: []string{"insecureHttp"}, value: p.InsecureHTTP},
		{path: []string{"dataDir"}, value: p.DataDir},
		{path: []string{"workspaceRoot"}, value: p.WorkspaceRoot},
		{path: []string{"debug"}, value: p.Debug},

		{path: []string{"github", "clientId"}, value: p.GitHubClientID},
		{path: []string{"github", "clientSecret"}, value: p.GitHubClientSecret},
		{path: []string{"github", "allowedUsers"}, value: p.AllowedUsers},
		{path: []string{"github", "apiUrl"}, value: p.GitHubAPIURL},

		{path: []string{"bitbucket", "apiUrl"}, value: p.BitbucketAPIURL},

		{path: []string{"claude", "credentials"}, value: p.ClaudeCredentials, keepEmpty: true},
		{path: []string{"claude", "anthropicApiKey"}, value: p.AnthropicAPIKey},
		{path: []string{"claude", "binary"}, value: p.ClaudeBinary},
		{path: []string{"claude", "model"}, value: p.ClaudeModel},

		{path: []string{"git", "userName"}, value: p.GitUserName},
		{path: []string{"git", "userEmail"}, value: p.GitUserEmail},

		{path: []string{"vscode", "dir"}, value: p.VSCodeDir},
		{path: []string{"vscode", "version"}, value: p.VSCodeVersion},

		{path: []string{"docker", "host"}, value: p.DockerHost},
		{path: []string{"docker", "cli"}, value: p.DockerCLI},

		{path: []string{"limits", "maxSessionsPerUser"}, value: p.MaxSessionsPerUser},
		{path: []string{"limits", "maxConcurrentBuilds"}, value: p.MaxConcurrentBuilds},
		{path: []string{"limits", "publicRatePerMinute"}, value: p.PublicRatePerMinute},
	}
}

// Update rewrites the configuration file at path with the settings in p and
// leaves everything else in it exactly as it found it. It returns the
// configuration the next start will resolve from the file, which is also what
// the caller applies to the running server.
//
// The edit is made on the decoded JSON document rather than on the file struct,
// so a file keeps the keys it had and gains no others: one that sets three
// things goes on setting three things instead of growing every key Hexagon
// knows at its default. Nothing can be lost that way — loadFile rejects unknown
// keys, so a file that loads holds nothing this package does not understand —
// but the keys come back in alphabetical order, which is the one visible cost.
//
// A setting given an empty value loses its key rather than gaining an empty
// one, for the same reason: an empty string, a false and an absent key already
// resolve identically. claude.credentials is the exception, and says so where
// it is declared.
func Update(path string, p Patch) (*Config, error) {
	return apply(path, p, true)
}

// Check resolves the configuration the file at path would have after p, without
// writing anything.
//
// It is how a caller refuses a save it could not apply — an allowlist that
// would lock the caller out, settings that would leave the server with no login
// at all — before the file is replaced rather than after it, which is the
// difference between a settings page that can be wrong and one that can leave a
// server nobody can get into.
func Check(path string, p Patch) (*Config, error) {
	return apply(path, p, false)
}

// apply builds the patched document, proves it loads, and replaces the file
// when commit says so.
//
// The candidate goes to a temporary file in the target's directory and Load is
// asked to read it before anything is renamed. That is what makes a form safe:
// the file is checked by the very function that will read it at the next start,
// so the permissions, the unknown keys, the addr/publicUrl pair, the bounds on
// every limit and the secret key are all enforced by their originals rather
// than by a second copy of the rules. A refusal costs the temporary file and
// nothing else. Load resolves with this process's environment, which is right:
// the next start has the same one.
func apply(path string, p Patch, commit bool) (*Config, error) {
	if path == "" {
		return nil, errors.New("no configuration file to write: name one with -config or HEXAGON_CONFIG")
	}

	// Refuse to touch a file that does not load at all. The candidate below
	// would be refused anyway, but this names the file the operator has rather
	// than the one they asked for.
	if _, _, err := loadFile(path, false); err != nil {
		return nil, err
	}

	doc, err := readDocument(path)
	if err != nil {
		return nil, err
	}
	if p.ClaudeCredentialsDefault {
		remove(doc, []string{"claude", "credentials"})
	}
	for _, field := range p.fields() {
		value, ok := deref(field.value)
		switch {
		case !ok:
		case isEmpty(value) && !field.keepEmpty:
			remove(doc, field.path)
		default:
			set(doc, field.path, value)
		}
	}
	pruneGroups(doc, p.fields())

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", path, err)
	}

	tmp, err := writeCandidate(path, append(out, '\n'))
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)

	cfg, err := Load(tmp)
	if err != nil {
		return nil, err
	}
	// Load saw the temporary file; the caller is entitled to the path it asked
	// about, and nothing outside this function should ever learn the other one.
	cfg.ConfigFile, cfg.ConfigPath = path, path

	if commit {
		if err := os.Rename(tmp, path); err != nil {
			return nil, fmt.Errorf("replace %s: %w", path, err)
		}
	}
	return cfg, nil
}

// set writes value at path, creating the groups on the way.
func set(doc map[string]any, path []string, value any) {
	group := doc
	for _, key := range path[:len(path)-1] {
		child, _ := group[key].(map[string]any)
		if child == nil {
			// Either the group is not there or it is not an object. The second
			// is unreachable on a file that loads — loadFile decodes it into a
			// struct — so replacing it is the harmless branch.
			child = map[string]any{}
			group[key] = child
		}
		group = child
	}
	group[path[len(path)-1]] = value
}

// remove deletes the key at path. A group that is not there is one the key is
// already absent from.
func remove(doc map[string]any, path []string) {
	group := doc
	for _, key := range path[:len(path)-1] {
		child, ok := group[key].(map[string]any)
		if !ok {
			return
		}
		group = child
	}
	delete(group, path[len(path)-1])
}

// pruneGroups drops a group the patch emptied, so a file does not accumulate
// "github": {} as its settings are cleared one by one. Only groups this patch
// touched are considered: an empty group somebody wrote by hand is theirs.
func pruneGroups(doc map[string]any, fields []patchField) {
	for _, field := range fields {
		if len(field.path) < 2 {
			continue
		}
		if group, ok := doc[field.path[0]].(map[string]any); ok && len(group) == 0 {
			delete(doc, field.path[0])
		}
	}
}

// deref unwraps a patch field's typed pointer, reporting false when the caller
// left it alone.
func deref(value any) (any, bool) {
	switch v := value.(type) {
	case *string:
		if v == nil {
			return nil, false
		}
		return *v, true
	case *bool:
		if v == nil {
			return nil, false
		}
		return *v, true
	case *int:
		if v == nil {
			return nil, false
		}
		return *v, true
	case *[]string:
		if v == nil {
			return nil, false
		}
		// Never null: the file is read back by people, and an empty list says
		// "nobody" where null says nothing at all.
		if *v == nil {
			return []string{}, true
		}
		return *v, true
	}
	panic(fmt.Sprintf("config: patch field of unsupported type %T", value))
}

func isEmpty(value any) bool {
	switch v := value.(type) {
	case string:
		return v == ""
	case bool:
		return !v
	case int:
		return v == 0
	case []string:
		return len(v) == 0
	}
	return false
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

// writeCandidate puts data in a temporary file beside path, with the
// permissions the loader demands, and returns its name. The caller either
// renames it over path or removes it.
func writeCandidate(path string, data []byte) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	// CreateTemp opens with 0600, which is what the file has to end up with:
	// it holds the OAuth client secret, and it must never exist with wider
	// permissions, even for the moment between writing it and renaming it. The
	// same directory, so the rename is atomic rather than a copy across
	// filesystems.
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return "", fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	return tmp.Name(), nil
}

// Writable reports whether Update could write the file at path.
//
// It answers by trying, because on a POSIX system nothing else is conclusive:
// the mode bits say little about a directory reached through a read-only mount,
// or owned by another user, or covered by an ACL. Nothing survives the check:
// the probe is a temporary file, made and removed.
//
// The probe is the *directory* and never the file, because that is what Update
// needs. It replaces the configuration by writing a candidate beside it and
// renaming it over the old one, and neither half asks anything of the file's own
// mode. A permission check that opened the file would answer yes for the layout
// the packages install — a 0600 file the service owns, in a directory it does
// not — and the settings page would offer a form that fails at the save.
func Writable(path string) bool {
	if path == "" {
		return false
	}
	// The directory may not exist — on a machine that has never had a
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
