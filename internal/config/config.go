// Package config stores TailLAN's small amount of local state: the
// player's display name, their client identity key, and (only when the
// host asks to keep an invite code) saved server keys.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"tailscale.com/types/key"
)

// Dir returns TailLAN's config directory, creating nothing.
// Override with $TAILLAN_HOME (handy for running two copies on one
// machine to test).
func Dir() string {
	if d := os.Getenv("TAILLAN_HOME"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "taillan")
}

// GamesDir is where user game profiles live.
func GamesDir() string { return filepath.Join(Dir(), "games") }

// Settings are the user's preferences.
type Settings struct {
	// Name is shown to friends ("Lane is hosting Valheim").
	Name string `json:"name"`
	// ClientKey is this machine's identity when joining, so hosts
	// that lock their room recognise returning friends.
	ClientKey string `json:"clientKey,omitempty"`
	// Verbose enables tunnel debug logs in the Logs panel.
	Verbose bool `json:"verbose,omitempty"`
}

var mu sync.Mutex

func settingsPath() string { return filepath.Join(Dir(), "settings.json") }

// Load reads settings, filling defaults (and generating a client key)
// on first run.
func Load() (*Settings, error) {
	mu.Lock()
	defer mu.Unlock()
	s := &Settings{}
	data, err := os.ReadFile(settingsPath())
	if err == nil {
		if err := json.Unmarshal(data, s); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	changed := false
	if strings.TrimSpace(s.Name) == "" {
		s.Name = defaultName()
		changed = true
	}
	if s.ClientKey == "" {
		b, _ := key.NewNode().MarshalText()
		s.ClientKey = string(b)
		changed = true
	}
	if changed {
		if err := saveLocked(s); err != nil {
			return s, nil // still usable in memory
		}
	}
	return s, nil
}

// Save writes settings.
func Save(s *Settings) error {
	mu.Lock()
	defer mu.Unlock()
	return saveLocked(s)
}

func saveLocked(s *Settings) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(settingsPath(), data, 0o600)
}

// ClientPrivateKey returns the parsed client key, or a fresh one if
// the stored key is unusable.
func (s *Settings) ClientPrivateKey() key.NodePrivate {
	var k key.NodePrivate
	if err := k.UnmarshalText([]byte(s.ClientKey)); err != nil || k.IsZero() {
		return key.NewNode()
	}
	return k
}

// CleanName tidies a display name: single spaces, and at most 40
// characters. An empty result means no name was given.
func CleanName(n string) string {
	n = strings.Join(strings.Fields(n), " ")
	if r := []rune(n); len(r) > 40 {
		n = strings.TrimSpace(string(r[:40]))
	}
	return n
}

// systemUsers are account names that say nothing about who's playing,
// such as a container's or a server's default user.
var systemUsers = map[string]bool{"root": true, "nonroot": true, "nobody": true}

// containerID matches a Docker container's default host name.
var containerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

func defaultName() string {
	if u, err := user.Current(); err == nil {
		n := u.Username
		if i := strings.LastIndexAny(n, `\/`); i >= 0 { // Windows DOMAIN\user
			n = n[i+1:]
		}
		if n != "" && !systemUsers[strings.ToLower(n)] {
			return strings.ToUpper(n[:1]) + n[1:]
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" && !containerID.MatchString(h) {
		return h
	}
	return "Player"
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// HostKey is a saved server identity that keeps a host's invite code
// the same across sessions.
type HostKey struct {
	Key      string `json:"key"`      // key.NodePrivate text
	PSK      string `json:"psk"`      // tailcat.PresharedKey text
	RegionID int    `json:"regionID"` // DERP region pinned at first use
}

func hostKeyPath(name string) string {
	return filepath.Join(Dir(), "keys", name+".json")
}

// LoadHostKey returns the saved key for name, or nil if none.
func LoadHostKey(name string) (*HostKey, error) {
	data, err := os.ReadFile(hostKeyPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var hk HostKey
	if err := json.Unmarshal(data, &hk); err != nil {
		return nil, err
	}
	return &hk, nil
}

// SaveHostKey stores a host key under name. The file holds a secret:
// anyone with it can impersonate this host's invite.
func SaveHostKey(name string, hk *HostKey) error {
	p := hostKeyPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(hk, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, data, 0o600)
}

// DeleteHostKey forgets a saved host key, retiring its invite code.
func DeleteHostKey(name string) error {
	err := os.Remove(hostKeyPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
