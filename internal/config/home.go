// Package config reads and writes connections.toml and replication files.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Home is the directory holding connections.toml and replications/*.toml:
// TUMMY_HOME, else XDG_CONFIG_HOME/tummy, else ~/.config/tummy.
func Home() string {
	if h := os.Getenv("TUMMY_HOME"); h != "" {
		return h
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "tummy")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tummy")
}

// ConnectionsPath is TUMMY_CONNECTIONS, else HOME/connections.toml.
func ConnectionsPath(home string) string {
	if p := os.Getenv("TUMMY_CONNECTIONS"); p != "" {
		return p
	}
	return filepath.Join(home, "connections.toml")
}

// EnvFile is HOME/tummy.env.
func EnvFile(home string) string { return filepath.Join(home, "tummy.env") }

func ReplicationsDir(home string) string { return filepath.Join(home, "replications") }

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidName reports whether s can name a replication or connection: letters, digits, _ and -.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ReplicationPath is HOME/replications/NAME.toml.
func ReplicationPath(home, name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("replication name %q: letters, digits, _ and - only", name)
	}
	return filepath.Join(ReplicationsDir(home), name+".toml"), nil
}

// ListReplicationFiles returns every *.toml under home/replications, sorted.
func ListReplicationFiles(home string) ([]string, error) {
	entries, err := os.ReadDir(ReplicationsDir(home))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, filepath.Join(ReplicationsDir(home), e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// NameOf is the replication name of a file: its base name without .toml.
func NameOf(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// ResolvePath resolves a sqlite path: ~ is the user's home, a relative path is relative
// to dir. ":memory:" and "file:" URIs are returned as given.
func ResolvePath(p, dir string) string {
	switch {
	case p == "" || p == ":memory:" || strings.HasPrefix(p, "file:"):
		return p
	case p == "~" || strings.HasPrefix(p, "~/"):
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
		return p
	case filepath.IsAbs(p) || dir == "":
		return p
	}
	return filepath.Join(dir, p)
}

// SetEnabled rewrites the top-level enabled key of a replication file in place,
// keeping everything else as it is.
func SetEnabled(path string, enabled bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text, err := setTopKey(string(raw), "enabled", strconv.FormatBool(enabled))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return writeFile(path, text)
}

// DeleteReplication removes HOME/replications/NAME.toml.
func DeleteReplication(home, name string) error {
	p, err := ReplicationPath(home, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// writeFile replaces path atomically. A new file is 0600 in a 0700 directory; an existing
// file keeps its mode.
func writeFile(path, text string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
