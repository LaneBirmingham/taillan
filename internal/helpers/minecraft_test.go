package helpers

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/LaneBirmingham/taillan/internal/portscan"
	"github.com/LaneBirmingham/taillan/internal/profile"
)

func TestParseMCAnnounce(t *testing.T) {
	motd, port, ok := parseMCAnnounce("[MOTD]Lane - World[/MOTD][AD]53421[/AD]")
	if !ok || motd != "Lane - World" || port != 53421 {
		t.Errorf("got %q %d %v", motd, port, ok)
	}
	for _, bad := range []string{"", "[MOTD]x[/MOTD]", "[MOTD]x[/MOTD][AD]99999[/AD]"} {
		if _, _, ok := parseMCAnnounce(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestMCPortOwner(t *testing.T) {
	socks := []portscan.Socket{
		{Proto: "tcp", Port: 53421, Process: "javaw.exe"},
		{Proto: "tcp", Port: 22, Process: "sshd"},
		{Proto: "udp", Port: 4445, Process: "java"},
		{Proto: "tcp", Port: 25565, Process: "taillan", Self: true},
	}
	if err := mcPortOwner(socks, 53421); err != nil {
		t.Errorf("java listener rejected: %v", err)
	}
	for _, port := range []uint16{22, 4445, 25565, 9999} {
		if err := mcPortOwner(socks, port); err == nil {
			t.Errorf("port %d accepted as a Minecraft world", port)
		}
	}
}

func TestMCSafeName(t *testing.T) {
	cases := map[string]string{
		"Lane":                "Lane",
		"":                    "Friend",
		"x[/MOTD][AD]22[/AD]": "x/MOTDAD22/AD",
		"[]":                  "Friend",
		"a\nb":                "ab",
	}
	for in, want := range cases {
		got := mcSafeName(in)
		if got != want {
			t.Errorf("mcSafeName(%q) = %q, want %q", in, got, want)
		}
		if _, port, ok := parseMCAnnounce("[MOTD]" + got + "'s world[/MOTD][AD]40000[/AD]"); !ok || port != 40000 {
			t.Errorf("name %q changes the advertised port: %d %v", in, port, ok)
		}
	}
}

// TestMinecraftHostDetects sends Open-to-LAN announcements like
// Minecraft does and checks the host helper retargets the game port,
// but only to a port that passes mcCheckPort.
func TestMinecraftHostDetects(t *testing.T) {
	if testing.Short() {
		t.Skip("uses multicast")
	}
	check := mcCheckPort
	defer func() { mcCheckPort = check }()
	mcCheckPort = func(_ context.Context, port uint16) error {
		if port == 53421 {
			return nil
		}
		return errors.New("not Minecraft")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := make(chan uint16, 1)
	env := &HostEnv{
		Profile: &profile.Profile{Ports: []profile.Port{{Port: 25565, Proto: "tcp"}}},
		SetTarget: func(g int, port uint16, note string) {
			select {
			case got <- port:
			default:
			}
		},
		Logf: t.Logf,
	}
	go minecraftLAN{}.Host(ctx, env)
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pc := ipv4.NewPacketConn(c)
	pc.SetMulticastLoopback(true)
	ifs := multicastIfaces()
	if len(ifs) == 0 {
		t.Skip("no multicast interfaces")
	}
	for {
		for _, ifi := range ifs {
			pc.SetMulticastInterface(&ifi)
			// A forged announcement for another port first: it must be ignored.
			pc.WriteTo([]byte("[MOTD]Fake[/MOTD][AD]22[/AD]"), nil, mcLANGroup)
			pc.WriteTo([]byte("[MOTD]Lane - World[/MOTD][AD]53421[/AD]"), nil, mcLANGroup)
		}
		select {
		case p := <-got:
			if p != 53421 {
				t.Fatalf("target = %d", p)
			}
			return
		case <-ctx.Done():
			t.Fatal("announcement not detected")
		case <-time.After(300 * time.Millisecond):
		}
	}
}
