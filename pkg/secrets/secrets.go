// Package secrets manages mcprt credentials in a chmod-600 KEY=VALUE file.
// The file path is set via secrets_file in mcprt.toml. Writes are atomic
// (write to temp, rename) so a crash during save never corrupts the file.
package secrets

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store reads and writes a chmod-600 KEY=VALUE secrets file.
type Store struct {
	path string
}

// Open opens (or creates) the secrets file at path with 0600 permissions.
func Open(path string) (*Store, error) {
	path = expandHome(path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("creating secrets dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening secrets file %q: %w", path, err)
	}
	f.Close()
	_ = os.Chmod(path, 0600)
	return &Store{path: path}, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Get(key string) (string, error) {
	all, err := s.readAll()
	if err != nil {
		return "", err
	}
	v, ok := all[key]
	if !ok {
		return "", fmt.Errorf("%s: not found", key)
	}
	return v, nil
}

func (s *Store) Set(key, value string) error {
	all, err := s.readAll()
	if err != nil {
		return err
	}
	all[key] = value
	return s.writeAll(all)
}

func (s *Store) Delete(key string) error {
	all, err := s.readAll()
	if err != nil {
		return err
	}
	if _, ok := all[key]; !ok {
		return fmt.Errorf("%s: not found", key)
	}
	delete(all, key)
	return s.writeAll(all)
}

func (s *Store) List() ([]string, error) {
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *Store) LoadAll() (map[string]string, error) {
	return s.readAll()
}

func (s *Store) readAll() (map[string]string, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, scanner.Err()
}

func (s *Store) writeAll(vars map[string]string) error {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_ = os.Chmod(tmp.Name(), 0600)

	for _, k := range keys {
		v := vars[k]
		if strings.ContainsAny(v, " \t\"'\\#") {
			v = `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
		}
		if _, err := fmt.Fprintf(tmp, "%s=%s\n", k, v); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// MergeFile reads KEY=VALUE pairs from srcPath into vars without overwriting
// existing keys — used to layer a secondary file on top of the primary store.
func MergeFile(srcPath string, vars map[string]string) error {
	srcPath = expandHome(srcPath)
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if _, exists := vars[k]; !exists {
			vars[k] = v
		}
	}
	return scanner.Err()
}

func expandHome(s string) string {
	if !strings.HasPrefix(s, "~/") {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return filepath.Join(home, s[2:])
}
