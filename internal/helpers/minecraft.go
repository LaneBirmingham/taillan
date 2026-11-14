package helpers

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/ipv4"
)

// Minecraft Java Edition announces "Open to LAN" worlds by multicasting
// "[MOTD]<text>[/MOTD][AD]<port>[/AD]" to 224.0.2.60:4445 every ~1.5s.
// Clients list them under "LAN worlds" and connect to the packet's
// source IP plus the advertised port.
var mcLANGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 2, 60), Port: 4445}

const mcMarker = "(via TailLAN)"

var mcAnnounceRE = regexp.MustCompile(`\[MOTD\](.*?)\[/MOTD\]\[AD\](\d+)\[/AD\]`)

func init() { Register(minecraftLAN{}) }

type minecraftLAN struct{}

func (minecraftLAN) Name() string { return "minecraft-lan" }

func (minecraftLAN) Describe() string {
	return "Finds your Open-to-LAN world's port automatically, and lists the shared world under friends' LAN worlds."
}

// Host listens for this machine's own LAN-world announcements and
// points the game port at whatever port Minecraft picked.
func (minecraftLAN) Host(ctx context.Context, env *HostEnv) {
	// ListenMulticastUDP shares the port (SO_REUSEADDR), so the host's
	// own Minecraft can still listen for LAN worlds too.
	c, err := net.ListenMulticastUDP("udp4", nil, mcLANGroup)
	if err != nil {
		env.Logf("minecraft-lan: can't listen for LAN worlds: %v", err)
		return
	}
	go func() { <-ctx.Done(); c.Close() }()
	// It joined the group on one interface; Minecraft announces on
	// whichever it likes (Ethernet, Wi-Fi, VPN...), so join them all.
	pc := ipv4.NewPacketConn(c)
	for _, ifi := range multicastIfaces() {
		pc.JoinGroup(&ifi, mcLANGroup) // "already joined" errors are fine
	}
	local := localIPs()
	var lastPort uint16
	buf := make([]byte, 1500)
	for {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		motd, port, ok := parseMCAnnounce(string(buf[:n]))
		if !ok || strings.Contains(motd, mcMarker) {
			continue // not a world, or a friend's TailLAN re-announcement
		}
		if !local[src.IP.String()] {
			local = localIPs() // interfaces may have changed
			if !local[src.IP.String()] {
				continue // someone else's world on the LAN
			}
		}
		if port != lastPort {
			lastPort = port
			env.SetTarget(0, port, fmt.Sprintf("Found your LAN world %q on port %d", motd, port))
		}
	}
}

// Join re-announces the forwarded world locally so it shows up in
// Multiplayer under LAN worlds. Minecraft connects to the packet's
// source address (this machine's LAN IP), which is why the profile
// marks the port "lan".
func (minecraftLAN) Join(ctx context.Context, env *JoinEnv) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		env.Logf("minecraft-lan: can't announce LAN world: %v", err)
		return
	}
	defer c.Close()
	pc := ipv4.NewPacketConn(c)
	pc.SetMulticastLoopback(true)
	pc.SetMulticastTTL(1)
	name := env.HostName
	if name == "" {
		name = "Friend"
	}
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	noted := false
	for {
		msg := []byte(fmt.Sprintf("[MOTD]%s's world %s[/MOTD][AD]%d[/AD]", name, mcMarker, env.LocalPort(0)))
		// Announce on every interface: Minecraft listens on whichever
		// its OS considers default, and connects back to the source
		// address, which the forward accepts from any of our own IPs.
		sent := false
		for _, ifi := range multicastIfaces() {
			if pc.SetMulticastInterface(&ifi) == nil {
				if _, err := pc.WriteTo(msg, nil, mcLANGroup); err == nil {
					sent = true
				}
			}
		}
		if !sent {
			_, err := c.WriteToUDP(msg, mcLANGroup)
			sent = err == nil
		}
		if sent && !noted {
			env.Note("Listed under LAN worlds in Minecraft's Multiplayer screen")
			noted = true
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func parseMCAnnounce(s string) (motd string, port uint16, ok bool) {
	m := mcAnnounceRE.FindStringSubmatch(s)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n < 1 || n > 65535 {
		return "", 0, false
	}
	return m[1], uint16(n), true
}

// multicastIfaces lists up, multicast-capable IPv4 interfaces.
func multicastIfaces() []net.Interface {
	ifs, _ := net.Interfaces()
	var out []net.Interface
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ifi)
				break
			}
		}
	}
	return out
}

func localIPs() map[string]bool {
	out := map[string]bool{"127.0.0.1": true}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out[ipn.IP.String()] = true
		}
	}
	return out
}
