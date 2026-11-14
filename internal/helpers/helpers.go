// Package helpers holds game-specific automations that a profile can
// opt into by name ("helpers": ["minecraft-lan"]).
//
// Most games need no helper: a profile's ports and instructions are
// enough. A helper is for the last bit of friction a JSON file can't
// remove, such as detecting a port that changes every session or
// making a forwarded game show up in a LAN server browser.
//
// To add one, implement [Helper] in a new file in this package and
// call [Register] from an init function; see minecraft.go and
// docs/helpers.md.
package helpers

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/LaneBirmingham/taillan/internal/profile"
)

// Helper is a game-specific automation.
type Helper interface {
	// Name is the id profiles use to enable the helper.
	Name() string
	// Describe says, in one sentence, what the helper does for the
	// player. It's shown in the UI.
	Describe() string
	// Host runs on the hosting player's machine for the whole
	// session, until ctx is cancelled. It may be a no-op.
	Host(ctx context.Context, env *HostEnv)
	// Join runs on each joining player's machine while connected,
	// until ctx is cancelled. It may be a no-op.
	Join(ctx context.Context, env *JoinEnv)
}

// HostEnv is what a helper can see and do on the host.
type HostEnv struct {
	Profile  *profile.Profile
	HostName string
	// SetTarget points a port group (an index into Profile.Ports) at
	// a different local port on the host, e.g. a freshly detected
	// Minecraft LAN port. note is shown to the host.
	SetTarget func(group int, port uint16, note string)
	Logf      func(format string, args ...any)
}

// JoinEnv is what a helper can see on a joining machine.
type JoinEnv struct {
	Profile  *profile.Profile
	HostName string
	// LocalPort returns the port a port group (an index into
	// Profile.Ports) is forwarded from on this machine.
	LocalPort func(group int) uint16
	// Note shows a status line to the joining player.
	Note func(note string)
	Logf func(format string, args ...any)
}

var (
	mu       sync.Mutex
	registry = map[string]Helper{}
)

// Register makes a helper available to profiles. Call it from init.
func Register(h Helper) {
	mu.Lock()
	defer mu.Unlock()
	registry[h.Name()] = h
}

// Get returns the named helper, or nil.
func Get(name string) Helper {
	mu.Lock()
	defer mu.Unlock()
	return registry[name]
}

// Info describes a registered helper for listings.
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// List returns all registered helpers, sorted by name.
func List() []Info {
	mu.Lock()
	defer mu.Unlock()
	var out []Info
	for _, h := range registry {
		out = append(out, Info{h.Name(), h.Describe()})
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// For returns the helpers a profile asks for that this build knows,
// and the names it doesn't.
func For(p *profile.Profile) (known []Helper, unknown []string) {
	for _, n := range p.Helpers {
		if h := Get(n); h != nil {
			known = append(known, h)
		} else {
			unknown = append(unknown, n)
		}
	}
	return known, unknown
}
