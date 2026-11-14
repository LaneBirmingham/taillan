// Package profile defines game profiles: small JSON files that tell
// TailLAN which ports a game uses and how players host and join it.
//
// Built-in profiles live in the repository's games/ directory and are
// embedded in the binary. Users add their own by dropping JSON files
// into the games folder under their config directory (see
// [UserDir]), by saving one from the UI, or with `taillan game new`.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ControlPort is the tunnel port TailLAN reserves for its own
// host<->friend control channel (names, latency, "host ended the
// session"). It exists only inside the encrypted tunnel's virtual
// network, never on a real interface, but game profiles must not use
// it.
const ControlPort = 65531

// MaxMappings bounds how many individual port forwards one profile may
// expand to, so a typo like "1-65535" can't open thousands of sockets.
const MaxMappings = 64

// Profile describes one game.
type Profile struct {
	// Schema optionally points editors at docs/game.schema.json for
	// autocomplete; TailLAN ignores it.
	Schema string `json:"$schema,omitempty"`
	// ID is a stable, lowercase identifier ("minecraft-java"). It
	// names the file and matches invites to local profiles.
	ID string `json:"id"`
	// Name is the human-readable game name.
	Name string `json:"name"`
	// Aliases are extra search terms ("mc", "minecraft").
	Aliases []string `json:"aliases,omitempty"`
	// Ports are the ports the game's server listens on.
	Ports []Port `json:"ports"`
	// Connect is what a joining player types into the game, as a
	// template. Placeholders: {{host}} (always 127.0.0.1), {{port}}
	// (the first port, as remapped on the joiner's machine),
	// {{port2}}, {{port3}}... for later entries, and {{hostname}}
	// (the hosting player's display name). Defaults to
	// "{{host}}:{{port}}".
	Connect string `json:"connect,omitempty"`
	// Host and Join are short step-by-step instructions shown to the
	// hosting and joining players. Steps may use the same
	// placeholders as Connect, plus {{connect}}.
	Host Guide `json:"host"`
	Join Guide `json:"join"`
	// Helpers name optional built-in automations for this game, such
	// as "minecraft-lan" (detects the Open-to-LAN port on the host and
	// makes the world appear in friends' LAN list). Unknown helper
	// names are ignored with a warning, so profiles stay portable.
	Helpers []string `json:"helpers,omitempty"`
	// Notes are caveats worth knowing (crossplay must be off, etc.).
	Notes string `json:"notes,omitempty"`
	// Links point at official docs (dedicated server guides).
	Links []Link `json:"links,omitempty"`
	// Tags group games in the UI ("survival", "fps", "voice").
	Tags []string `json:"tags,omitempty"`

	// Source is where this profile came from: "builtin", "user" or
	// "invite". It is set by the loader, not read from JSON.
	Source string `json:"source,omitempty"`
	// Path is the file the profile was loaded from, if any.
	Path string `json:"path,omitempty"`
}

// Port is one port (or a contiguous range) a game listens on.
type Port struct {
	// Port is the first (or only) port number.
	Port uint16 `json:"port"`
	// End, if set, makes this an inclusive range Port..End
	// (Valheim's 2456-2457, for example). Ranges are shifted as a
	// unit when the joiner's machine already uses one of the ports,
	// preserving offsets games rely on (query port = game port + 1).
	End uint16 `json:"end,omitempty"`
	// Proto is "tcp", "udp" or "both".
	Proto string `json:"proto"`
	// Label says what the port is for ("Game", "Query", "RCON").
	Label string `json:"label,omitempty"`
	// Ask, if set, makes the host UI show an input for where the game
	// actually listens on the host's machine, with this text as its
	// label: for games whose server port changes each session, like
	// Minecraft's "Open to LAN". The tunnel side stays at Port, so
	// invites never change.
	Ask string `json:"ask,omitempty"`
	// SamePort means the joiner must use exactly this port locally,
	// because the game has no way to type a port. If it's busy on the
	// joiner's machine, joining fails with a clear message instead of
	// silently remapping.
	SamePort bool `json:"samePort,omitempty"`
	// LAN makes the joiner listen on all network interfaces (still
	// accepting only connections from the joiner's own machine), so
	// games that find servers by LAN broadcast can see the forwarded
	// game. Use it only when a game needs it: on Windows it triggers
	// a one-time firewall prompt.
	LAN bool `json:"lan,omitempty"`
}

// Guide is a list of instruction steps.
type Guide struct {
	Steps []string `json:"steps,omitempty"`
}

// Link is a titled URL.
type Link struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Mapping is one expanded port forward: a single port and protocol.
type Mapping struct {
	Proto    string `json:"proto"` // "tcp" or "udp"
	Port     uint16 `json:"port"`  // port inside the tunnel (= the profile's port)
	Group    int    `json:"group"` // index into Profile.Ports
	Label    string `json:"label,omitempty"`
	SamePort bool   `json:"samePort,omitempty"`
	LAN      bool   `json:"lan,omitempty"`
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Validate reports problems with p, all at once, so `taillan game
// validate` can show every mistake in one go.
func (p *Profile) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !idRE.MatchString(p.ID) {
		add("id %q must be lowercase letters, digits and dashes (e.g. \"my-game\")", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		add("name is required")
	}
	if len(p.Ports) == 0 {
		add("at least one port is required")
	}
	total := 0
	for i, pt := range p.Ports {
		where := fmt.Sprintf("ports[%d]", i)
		if pt.Port == 0 {
			add("%s: port must be 1-65535", where)
		}
		if pt.End != 0 && pt.End < pt.Port {
			add("%s: end (%d) is before port (%d)", where, pt.End, pt.Port)
		}
		switch pt.Proto {
		case "tcp", "udp", "both":
		default:
			add("%s: proto must be \"tcp\", \"udp\" or \"both\", not %q", where, pt.Proto)
		}
		last := pt.Port
		if pt.End > pt.Port {
			last = pt.End
		}
		if pt.Port <= ControlPort && ControlPort <= last {
			add("%s: port %d is reserved by TailLAN", where, ControlPort)
		}
		n := int(last) - int(pt.Port) + 1
		if pt.Proto == "both" {
			n *= 2
		}
		total += n
	}
	if total > MaxMappings {
		add("ports expand to %d forwards; the limit is %d", total, MaxMappings)
	}
	if dup := p.duplicateMapping(); dup != "" {
		add("port %s is listed twice", dup)
	}
	for i, l := range p.Links {
		if !strings.HasPrefix(l.URL, "https://") && !strings.HasPrefix(l.URL, "http://") {
			add("links[%d]: url must start with https://", i)
		}
	}
	return errors.Join(errs...)
}

func (p *Profile) duplicateMapping() string {
	seen := map[string]bool{}
	for _, m := range p.Mappings() {
		k := fmt.Sprintf("%d/%s", m.Port, m.Proto)
		if seen[k] {
			return k
		}
		seen[k] = true
	}
	return ""
}

// Mappings expands the profile's ports into individual forwards.
func (p *Profile) Mappings() []Mapping {
	var out []Mapping
	for gi, pt := range p.Ports {
		last := pt.Port
		if pt.End > pt.Port {
			last = pt.End
		}
		protos := []string{pt.Proto}
		if pt.Proto == "both" {
			protos = []string{"tcp", "udp"}
		}
		for port := int(pt.Port); port <= int(last) && port != 0; port++ {
			for _, proto := range protos {
				out = append(out, Mapping{
					Proto:    proto,
					Port:     uint16(port),
					Group:    gi,
					Label:    pt.Label,
					SamePort: pt.SamePort,
					LAN:      pt.LAN,
				})
			}
		}
	}
	return out
}

// ConnectTemplate returns p.Connect or the default.
func (p *Profile) ConnectTemplate() string {
	if strings.TrimSpace(p.Connect) != "" {
		return p.Connect
	}
	return "{{host}}:{{port}}"
}

// Matches reports whether q (lowercased) matches the profile's id,
// name, aliases or tags, for search.
func (p *Profile) Matches(q string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return true
	}
	hay := []string{p.ID, strings.ToLower(p.Name)}
	for _, a := range p.Aliases {
		hay = append(hay, strings.ToLower(a))
	}
	hay = append(hay, p.Tags...)
	for _, h := range hay {
		if strings.Contains(h, q) {
			return true
		}
	}
	return false
}

// PortSummary renders the ports compactly: "25565/tcp, 2456-2457/udp".
func (p *Profile) PortSummary() string {
	var parts []string
	for _, pt := range p.Ports {
		s := strconv.Itoa(int(pt.Port))
		if pt.End > pt.Port {
			s += "-" + strconv.Itoa(int(pt.End))
		}
		parts = append(parts, s+"/"+pt.Proto)
	}
	return strings.Join(parts, ", ")
}

// ParsePorts parses a port spec typed by a person:
//
//	"25565"               TCP and UDP 25565 (when unsure, forward both)
//	"7777/udp"            UDP only
//	"2456-2457/udp"       a range
//	"27015/both, 27020"   several, separated by commas or spaces
//
// "t", "u" and "b" abbreviate the protocols.
func ParsePorts(spec string) ([]Port, error) {
	spec = strings.NewReplacer(";", ",", "\n", ",", " ", ",").Replace(spec)
	var out []Port
	for f := range strings.SplitSeq(spec, ",") {
		f = strings.TrimSpace(strings.ToLower(f))
		if f == "" {
			continue
		}
		proto := "both"
		if num, pr, ok := strings.Cut(f, "/"); ok {
			f = num
			switch pr {
			case "tcp", "t":
				proto = "tcp"
			case "udp", "u":
				proto = "udp"
			case "both", "b", "tcp+udp", "udp+tcp", "all":
				proto = "both"
			default:
				return nil, fmt.Errorf("%q: protocol must be tcp, udp or both", pr)
			}
		}
		lo, hi, isRange := strings.Cut(f, "-")
		first, err := parsePort(lo)
		if err != nil {
			return nil, err
		}
		pt := Port{Port: first, Proto: proto}
		if isRange {
			last, err := parsePort(hi)
			if err != nil {
				return nil, err
			}
			if last < first {
				return nil, fmt.Errorf("range %s: end is before start", f)
			}
			if last > first {
				pt.End = last
			}
		}
		out = append(out, pt)
	}
	if len(out) == 0 {
		return nil, errors.New("no ports given")
	}
	return out, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port number (1-65535)", s)
	}
	return uint16(n), nil
}

// Parse decodes and validates one profile from JSON. Unknown fields
// are rejected so typos ("prot": "udp") don't silently do nothing.
func Parse(data []byte) (*Profile, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	p.Source, p.Path = "", ""
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Marshal renders p as indented JSON, without loader-only fields.
func (p *Profile) Marshal() ([]byte, error) {
	c := *p
	c.Source, c.Path, c.Schema = "", "", ""
	b, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Slug turns a game name into a profile id: "Don't Starve" -> "dont-starve".
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r == '\'' || r == '’':
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	if s == "" {
		s = "custom-game"
	}
	return s
}

// SortByName sorts profiles by display name, case-insensitively.
func SortByName(ps []*Profile) {
	slices.SortFunc(ps, func(a, b *Profile) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
}
