package invite

import (
	"strings"
	"testing"

	"github.com/LaneBirmingham/taillan/internal/profile"
)

const fakeAddr = "tcomFwWCCcjS5nKNqAod034nWoJZW0LZqDhhC8U_dKdnDRYQ8uNGFpGQEu-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-0123456789abcdefghijk"

func TestRoundTrip(t *testing.T) {
	p := &profile.Profile{
		ID: "valheim", Name: "Valheim", Source: "user",
		Ports:   []profile.Port{{Port: 2456, End: 2457, Proto: "udp", Label: "Game", SamePort: true}, {Port: 25565, Proto: "tcp", LAN: true}},
		Connect: "{{host}}:{{port}}",
		Join:    profile.Guide{Steps: []string{"Join IP → {{connect}}"}},
		Helpers: []string{"minecraft-lan"},
	}
	code, err := New(fakeAddr, "Lane", p).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, Prefix) || len(code) > 400 {
		t.Errorf("code %q (len %d)", code, len(code))
	}
	inv, err := Decode("hey! join with this:\n" + code + "\nsee you")
	if err != nil {
		t.Fatal(err)
	}
	got, err := inv.Profile()
	if err != nil {
		t.Fatal(err)
	}
	if inv.Addr != fakeAddr || inv.HostName != "Lane" || got.Name != "Valheim" ||
		got.Ports[0].End != 2457 || !got.Ports[0].SamePort || !got.Ports[1].LAN ||
		got.Join.Steps[0] != "Join IP → {{connect}}" || got.Helpers[0] != "minecraft-lan" {
		t.Errorf("round trip mismatch: %+v / %+v", inv, got)
	}
}

func TestBuiltinOmitsSteps(t *testing.T) {
	p := &profile.Profile{ID: "factorio", Name: "Factorio", Source: "builtin", Ports: []profile.Port{{Port: 34197, Proto: "udp"}},
		Join: profile.Guide{Steps: []string{"long instructions that the friend already has"}}}
	inv := New(fakeAddr, "", p)
	if len(inv.Steps) != 0 {
		t.Error("built-in steps were embedded")
	}
}

func TestDecodeErrors(t *testing.T) {
	for _, in := range []string{"", "hello", "TL1-!!!!!!!!!!!!!!!!!!!!!!!!!", "TL1-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", fakeAddr} {
		if _, err := Decode(in); err == nil {
			t.Errorf("Decode(%q) succeeded", in)
		}
	}
	// A tampered invite with a reserved port is rejected.
	p := &profile.Profile{ID: "x", Name: "X", Ports: []profile.Port{{Port: 80, Proto: "tcp"}}}
	inv := New(fakeAddr, "", p)
	inv.Ports[0].Port = profile.ControlPort
	code, _ := inv.Encode()
	if _, err := Decode(code); err == nil {
		t.Error("reserved port accepted")
	}
}
