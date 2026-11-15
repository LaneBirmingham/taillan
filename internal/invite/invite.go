// Package invite encodes everything a friend needs to join a game into
// one pasteable code: the host's tailcat address (which is also the
// secret that lets them in), the host's name, and the game's ports and
// join instructions. Friends don't need the game's profile installed.
//
// Format: "TL1-" + base64url(raw DEFLATE(JSON)).
package invite

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/LaneBirmingham/taillan/internal/profile"
)

// Prefix starts every invite code.
const Prefix = "TL1-"

// Invite is the decoded content of an invite code.
type Invite struct {
	Version  int      `json:"v"`
	Addr     string   `json:"a"`           // tailcat address
	HostName string   `json:"h,omitempty"` // display name of the host
	GameID   string   `json:"i,omitempty"`
	GameName string   `json:"n"`
	Ports    []Port   `json:"p"`
	Connect  string   `json:"c,omitempty"`
	Steps    []string `json:"s,omitempty"` // join steps (omitted for built-in games)
	Helpers  []string `json:"x,omitempty"`
	Notes    string   `json:"o,omitempty"`
}

// Port is the compact wire form of profile.Port.
type Port struct {
	Port  uint16 `json:"p"`
	End   uint16 `json:"e,omitempty"`
	Proto string `json:"t"` // "t", "u" or "b"
	Flags int    `json:"f,omitempty"`
	Label string `json:"l,omitempty"`
}

const (
	flagSamePort = 1 << iota
	flagLAN
)

// New builds an invite for a hosted profile. Join steps are included
// only for games the friend might not have built in.
func New(addr, hostName string, p *profile.Profile) *Invite {
	inv := &Invite{
		Version:  1,
		Addr:     addr,
		HostName: hostName,
		GameID:   p.ID,
		GameName: p.Name,
		Connect:  p.Connect,
		Helpers:  p.Helpers,
	}
	if p.Source != "builtin" {
		inv.Steps = p.Join.Steps
		inv.Notes = p.Notes
	}
	for _, pt := range p.Ports {
		wp := Port{Port: pt.Port, End: pt.End, Proto: pt.Proto[:1], Label: pt.Label}
		if pt.SamePort {
			wp.Flags |= flagSamePort
		}
		if pt.LAN {
			wp.Flags |= flagLAN
		}
		inv.Ports = append(inv.Ports, wp)
	}
	return inv
}

// Encode renders the invite code.
func (inv *Invite) Encode() (string, error) {
	js, err := json.Marshal(inv)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	w.Write(js)
	w.Close()
	return Prefix + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

var codeRE = regexp.MustCompile(`TL1-[A-Za-z0-9_-]{20,}`)

// Find extracts an invite code from pasted text, so friends can paste
// the whole chat message, not just the code.
func Find(text string) (string, bool) {
	m := codeRE.FindString(text)
	return m, m != ""
}

// Decode parses and sanity-checks an invite code (or text containing
// one). Invites come from other people, so everything is bounded.
func Decode(text string) (*Invite, error) {
	code, ok := Find(strings.TrimSpace(text))
	if !ok {
		if strings.HasPrefix(strings.TrimSpace(text), "tc") {
			return nil, errors.New("that's a raw tailcat address, not a TailLAN invite; ask your friend to send the TL1-... invite code")
		}
		return nil, errors.New("no invite code found; it starts with TL1-")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, Prefix))
	if err != nil {
		return nil, errors.New("invite code is damaged (was it cut off when copying?)")
	}
	js, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(raw)), 64<<10))
	if err != nil {
		return nil, errors.New("invite code is damaged (was it cut off when copying?)")
	}
	var inv Invite
	if err := json.Unmarshal(js, &inv); err != nil {
		return nil, fmt.Errorf("invite code is damaged: %v", err)
	}
	if inv.Version != 1 {
		return nil, fmt.Errorf("this invite needs a newer TailLAN (invite version %d)", inv.Version)
	}
	if !strings.HasPrefix(inv.Addr, "tc") {
		return nil, errors.New("invite has no host address")
	}
	// Same limit as the name a host can set for themselves.
	inv.HostName = clip(strings.TrimSpace(inv.HostName), 40)
	if _, err := inv.Profile(); err != nil {
		return nil, fmt.Errorf("invite has bad game info: %v", err)
	}
	return &inv, nil
}

// Profile reconstructs the game profile carried by the invite.
func (inv *Invite) Profile() (*profile.Profile, error) {
	p := &profile.Profile{
		ID:      inv.GameID,
		Name:    clip(inv.GameName, 120),
		Connect: clip(inv.Connect, 200),
		Helpers: inv.Helpers,
		Notes:   clip(inv.Notes, 1000),
		Source:  "invite",
	}
	if p.ID == "" {
		p.ID = profile.Slug(p.Name)
	}
	for i, s := range inv.Steps {
		if i == 12 {
			break
		}
		p.Join.Steps = append(p.Join.Steps, clip(s, 400))
	}
	for _, wp := range inv.Ports {
		pt := profile.Port{Port: wp.Port, End: wp.End, Label: clip(wp.Label, 60)}
		switch wp.Proto {
		case "t":
			pt.Proto = "tcp"
		case "u":
			pt.Proto = "udp"
		case "b":
			pt.Proto = "both"
		default:
			pt.Proto = wp.Proto // Validate rejects it
		}
		pt.SamePort = wp.Flags&flagSamePort != 0
		pt.LAN = wp.Flags&flagLAN != 0
		p.Ports = append(p.Ports, pt)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
