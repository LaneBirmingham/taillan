package helpers

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

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

// TestMinecraftHostDetects sends an Open-to-LAN announcement like
// Minecraft does and checks the host helper retargets the game port.
func TestMinecraftHostDetects(t *testing.T) {
	if testing.Short() {
		t.Skip("uses multicast")
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
