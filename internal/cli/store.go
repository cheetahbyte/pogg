package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

type Instance struct {
	Container string `json:"container,omitempty"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Admin     string `json:"admin"`
	SSLMode   string `json:"sslmode"`
}
type Database struct {
	Name     string `json:"database"`
	Owner    string `json:"owner"`
	Deleting bool   `json:"deleting,omitempty"`
}
type User struct {
	Name      string   `json:"username"`
	Databases []string `json:"databases"`
	SecretRef string   `json:"secretRef"`
}
type App struct {
	Deleting  bool                `json:"deleting,omitempty"`
	Instance  string              `json:"instance"`
	Databases map[string]Database `json:"databases"`
	Users     map[string]User     `json:"users"`
}
type State struct {
	Version   int                 `json:"version"`
	Default   string              `json:"defaultInstance,omitempty"`
	Instances map[string]Instance `json:"instances"`
	Apps      map[string]*App     `json:"apps"`
}

// Secrets keeps the read path independent of the optional future OS credential backend.
type Secrets interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}
type fileSecrets struct {
	path   string
	values map[string]string
}

func (s *fileSecrets) Get(k string) (string, error) {
	v, ok := s.values[k]
	if !ok {
		return "", fmt.Errorf("missing credential %q", k)
	}
	return v, nil
}
func (s *fileSecrets) Set(k, v string) error { s.values[k] = v; return atomicJSON(s.path, s.values) }
func (s *fileSecrets) Delete(k string) error {
	delete(s.values, k)
	return atomicJSON(s.path, s.values)
}

type store struct {
	dir     string
	state   State
	secrets Secrets
}

func openStore(dir string) (*store, func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	// ponytail: one fail-fast lock for all commands; use OS advisory locks if contention matters.
	lock := filepath.Join(dir, "lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("state locked (after a crash, remove %s only when no pogg or older pgplease process is running): %w", lock, err)
	}
	fmt.Fprintln(f, os.Getpid())
	f.Close()
	release := func() { os.Remove(lock) }
	s := &store{dir: dir, state: State{Version: 1, Instances: map[string]Instance{}, Apps: map[string]*App{}}}
	if err = loadJSON(filepath.Join(dir, "state.json"), &s.state); err != nil {
		release()
		return nil, nil, err
	}
	if s.state.Version != 1 || s.state.Instances == nil || s.state.Apps == nil {
		release()
		return nil, nil, errors.New("invalid or unsupported state")
	}
	if err = s.state.validate(); err != nil {
		release()
		return nil, nil, err
	}
	sec := &fileSecrets{path: filepath.Join(dir, "secrets.json"), values: map[string]string{}}
	if info, e := os.Stat(sec.path); e == nil && info.Mode().Perm()&0077 != 0 {
		release()
		return nil, nil, errors.New("secrets.json must have mode 0600")
	}
	if err = loadJSON(sec.path, &sec.values); err != nil || sec.values == nil {
		release()
		if err == nil {
			err = errors.New("invalid secrets file")
		}
		return nil, nil, err
	}
	s.secrets = sec
	return s, release, nil
}
func loadJSON(path string, v any) error {
	b, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return fmt.Errorf("invalid null document in %s", path)
	}
	if e = json.Unmarshal(b, v); e != nil {
		return fmt.Errorf("invalid %s: %w", path, e)
	}
	return nil
}
func atomicJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".pgplease-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(append(b, '\n')); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func (s *State) validate() error {
	if s.Default != "" {
		if _, ok := s.Instances[s.Default]; !ok {
			return errors.New("default instance missing from state")
		}
	}
	for name, i := range s.Instances {
		if checkInstanceName(name) != nil || i.Host == "" || i.Admin == "" || i.Port < 1 || i.Port > 65535 {
			return fmt.Errorf("invalid instance %q in state", name)
		}
	}
	for name, a := range s.Apps {
		if checkName(name) != nil || a == nil || a.Databases == nil || a.Users == nil {
			return fmt.Errorf("invalid app %q in state", name)
		}
		if _, ok := s.Instances[a.Instance]; !ok {
			return fmt.Errorf("app %q references an unknown instance", name)
		}
		for key, d := range a.Databases {
			want := makeDatabase(name, key)
			if checkName(key) != nil || d.Name != want.Name || d.Owner != want.Owner {
				return fmt.Errorf("invalid database %s/%s in state", name, key)
			}
		}
		for key, u := range a.Users {
			if checkName(key) != nil || u.Name != name+"_"+key || u.SecretRef != name+"/"+key || len(u.Databases) == 0 {
				return fmt.Errorf("invalid user %s/%s in state", name, key)
			}
			for _, db := range u.Databases {
				if _, ok := a.Databases[db]; !ok {
					return fmt.Errorf("user %s/%s references unknown database", name, key)
				}
			}
		}
	}
	return nil
}
func (s *store) save() error {
	if err := s.state.validate(); err != nil {
		return err
	}
	return atomicJSON(filepath.Join(s.dir, "state.json"), s.state)
}
func password() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

func checkName(s string) error {
	if !validName.MatchString(s) {
		return fmt.Errorf("invalid name %q: use 1–20 lowercase letters, digits or hyphens, starting with a letter", s)
	}
	return nil
}

var validInstanceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func checkInstanceName(s string) error {
	if !validInstanceName.MatchString(s) {
		return fmt.Errorf("invalid instance name %q", s)
	}
	return nil
}
func adminKey(name string) string { return "instance/" + name + "/admin" }
