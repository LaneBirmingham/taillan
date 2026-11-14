package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/LaneBirmingham/taillan/games"
)

// Store holds the built-in profiles plus the user's own, which
// override built-ins with the same id.
type Store struct {
	dir string // user profile directory

	mu       sync.Mutex
	builtin  map[string]*Profile
	problems []string // load errors, shown in the UI and `game list`
}

// NewStore loads built-in profiles and prepares to read user profiles
// from dir (created on first save).
func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir, builtin: map[string]*Profile{}}
	entries, err := fs.ReadDir(games.FS, ".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := fs.ReadFile(games.FS, e.Name())
		if err != nil {
			return nil, err
		}
		p, err := Parse(data)
		if err != nil {
			// Built-ins are checked by tests; a bad one is a bug.
			return nil, fmt.Errorf("built-in profile %s: %w", e.Name(), err)
		}
		p.Source = "builtin"
		s.builtin[p.ID] = p
	}
	return s, nil
}

// Dir is the user profile directory.
func (s *Store) Dir() string { return s.dir }

// All returns every profile, user profiles replacing built-ins with
// the same id, sorted by name. It re-reads the user directory each
// time so files dropped in by hand show up without a restart.
func (s *Store) All() []*Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := map[string]*Profile{}
	for id, p := range s.builtin {
		byID[id] = p
	}
	s.problems = nil
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		data, err := os.ReadFile(path)
		if err == nil {
			var p *Profile
			p, err = Parse(data)
			if err == nil {
				p.Source, p.Path = "user", path
				byID[p.ID] = p
				continue
			}
		}
		s.problems = append(s.problems, fmt.Sprintf("%s: %v", e.Name(), err))
	}
	out := make([]*Profile, 0, len(byID))
	for _, p := range byID {
		out = append(out, p)
	}
	SortByName(out)
	return out
}

// Problems returns errors from the last All call (bad user files).
func (s *Store) Problems() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.problems...)
}

// Get returns the profile with the given id, or nil.
func (s *Store) Get(id string) *Profile {
	for _, p := range s.All() {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Builtin returns the built-in profile with the given id, ignoring
// user overrides, or nil.
func (s *Store) Builtin(id string) *Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builtin[id]
}

// Find returns profiles matching a search query.
func (s *Store) Find(q string) []*Profile {
	var out []*Profile
	for _, p := range s.All() {
		if p.Matches(q) {
			out = append(out, p)
		}
	}
	return out
}

// Save validates p and writes it to the user directory as <id>.json,
// returning the path.
func (s *Store) Save(p *Profile) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	data, err := p.Marshal()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, p.ID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Delete removes a user profile. Built-ins can't be deleted (a user
// file overriding one can, which restores the built-in).
func (s *Store) Delete(id string) error {
	p := s.Get(id)
	if p == nil {
		return fmt.Errorf("no game %q", id)
	}
	if p.Source != "user" {
		return errors.New("built-in games can't be deleted")
	}
	return os.Remove(p.Path)
}
