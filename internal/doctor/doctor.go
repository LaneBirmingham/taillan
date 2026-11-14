// Package doctor checks whether this machine can host and join games:
// can it reach the relays, does UDP work, what kind of NAT is it
// behind, and does a full tunnel round trip work locally.
package doctor

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/net/netcheck"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"

	"github.com/LaneBirmingham/taillan/internal/invite"
	"github.com/LaneBirmingham/taillan/internal/portscan"
	"github.com/LaneBirmingham/taillan/internal/profile"
	"github.com/LaneBirmingham/taillan/internal/session"
)

// Check is one diagnostic result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok", "warn", "fail"
	Detail string `json:"detail"`
}

// Report is the full diagnosis.
type Report struct {
	Checks  []Check `json:"checks"`
	Summary string  `json:"summary"`
}

func (r *Report) add(name, status, format string, args ...any) {
	r.Checks = append(r.Checks, Check{name, status, fmt.Sprintf(format, args...)})
}

// Run performs all checks. It takes a few seconds.
func Run(ctx context.Context) *Report {
	r := &Report{}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	dm, err := tailcat.FetchDERPMap(ctx)
	if err != nil {
		r.add("Relay servers", "fail", "Can't download the relay list: %v. Check your internet connection and firewall.", err)
		r.Summary = "No internet access to the relay servers, so TailLAN can't connect anyone."
		return r
	}
	r.add("Relay servers", "ok", "Found %d relay regions.", len(dm.Regions))

	nr, err := netReport(ctx, dm)
	switch {
	case err != nil:
		r.add("Network check", "warn", "Couldn't run the network check: %v", err)
	default:
		best, bestLat := tailcfg.DERPRegionID(0), time.Hour
		for id, d := range nr.RegionLatency {
			if d < bestLat {
				best, bestLat = id, d
			}
		}
		if reg := dm.Regions[best]; reg != nil {
			r.add("Nearest relay", "ok", "%s, %v away.", reg.RegionName, bestLat.Round(time.Millisecond))
		}
		if nr.UDP {
			r.add("UDP", "ok", "UDP works, so direct (fast) connections are possible.")
		} else {
			r.add("UDP", "warn", "UDP seems blocked on this network (common on work, school and hotel Wi-Fi). Games will still connect through relays, but slower.")
		}
		switch nr.MappingVariesByDestIP {
		case "true":
			r.add("NAT type", "warn", "Your router uses a strict (\"hard\") NAT. Direct connections may fail with friends who also have one; traffic then goes through a relay, which adds lag. Enabling UPnP on your router, or playing from a different network, can help.")
		case "false":
			r.add("NAT type", "ok", "Your router's NAT is friendly to direct connections.")
		}
		if nr.UPnP == "true" || nr.PMP == "true" || nr.PCP == "true" {
			r.add("Router port mapping", "ok", "Your router supports automatic port mapping (UPnP/NAT-PMP).")
		}
	}

	if err := selfTest(ctx, r); err != nil {
		r.add("Tunnel self-test", "fail", "%v", err)
	}

	sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
	if _, err := portscan.Listening(sctx); err != nil {
		r.add("Port detection", "warn", "Can't list listening ports (%v). The port detector and \"game server running?\" checks won't work, but sharing still does.", err)
	} else {
		r.add("Port detection", "ok", "Can see which ports games listen on.")
	}
	scancel()

	r.Summary = "Everything looks good for hosting and joining."
	for _, c := range r.Checks {
		if c.Status == "fail" {
			r.Summary = "Something is blocking TailLAN; see the failed checks."
			break
		}
		if c.Status == "warn" {
			r.Summary = "TailLAN will work, with the caveats below."
		}
	}
	return r
}

func netReport(ctx context.Context, dm *tailcfg.DERPMap) (*netcheck.Report, error) {
	netns.SetEnabled(false) // as tailcat.PickBestRegion does; see its comment
	nc := &netcheck.Client{NetMon: netmon.NewStatic(), Logf: logger.Discard}
	if err := nc.Standalone(ctx, ":0"); err != nil {
		return nil, err
	}
	return nc.GetReport(ctx, dm, &netcheck.GetReportOpts{})
}

// selfTest hosts a tiny echo "game" and joins it from this same
// process, exercising relays, the tunnel and local forwarding.
func selfTest(ctx context.Context, r *Report) error {
	tl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer tl.Close()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	port := uint16(tl.Addr().(*net.TCPAddr).Port)
	game := &profile.Profile{ID: "self-test", Name: "Self-test", Ports: []profile.Port{{Port: port, Proto: "tcp"}}, Source: "user"}
	log := session.NewLogBuffer(nil)
	t0 := time.Now()
	h, err := session.StartHost(ctx, session.HostConfig{Profile: game, HostName: "self-test", Log: log})
	if err != nil {
		return fmt.Errorf("couldn't start a test host: %v", err)
	}
	defer h.Stop()
	inv, err := invite.Decode(h.Invite())
	if err != nil {
		return err
	}
	jp, _ := session.JoinProfile(inv, nil)
	j, err := session.StartJoin(session.JoinConfig{Invite: inv, Profile: jp, Name: "self-test", Key: key.NewNode(), Log: log})
	if err != nil {
		return fmt.Errorf("couldn't start a test join: %v", err)
	}
	defer j.Stop()
	for j.Status().Phase != "connected" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the test connection never came up: %s", j.Status().Message)
		case <-time.After(200 * time.Millisecond):
		}
	}
	local := ""
	for _, p := range j.Status().Ports {
		local = p.Local
	}
	c, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	msg := []byte("taillan self-test " + strconv.FormatInt(time.Now().UnixNano(), 10))
	if _, err := c.Write(msg); err != nil {
		return err
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != string(msg) {
		return fmt.Errorf("data didn't make it through the tunnel: %v", err)
	}
	r.add("Tunnel self-test", "ok", "Hosted and joined a test game through the encrypted tunnel in %v.", time.Since(t0).Round(100*time.Millisecond))
	return nil
}
