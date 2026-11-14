package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"

	"github.com/LaneBirmingham/taillan/internal/config"
	"github.com/LaneBirmingham/taillan/internal/helpers"
	"github.com/LaneBirmingham/taillan/internal/invite"
	"github.com/LaneBirmingham/taillan/internal/portscan"
	"github.com/LaneBirmingham/taillan/internal/profile"
)

// HostConfig configures a hosting session.
type HostConfig struct {
	// Profile is the game being shared, including any extra ports
	// the host added.
	Profile *profile.Profile
	// TargetIP is where the game server runs, as seen from the host:
	// 127.0.0.1 (the default) or another device on the host's LAN.
	TargetIP string
	// Targets overrides, per port group, the host-side port the game
	// actually listens on (for profiles with "ask" ports).
	Targets map[int]uint16
	// KeepCode reuses a saved identity so the invite stays the same
	// across sessions (and saves one if needed). Otherwise every
	// session gets a fresh, single-use invite.
	KeepCode bool
	// HostName is shown to friends.
	HostName string
	// Verbose sends tailcat's own debug logging to Log.
	Verbose bool
	Log     *LogBuffer
}

// Host is a running hosting session.
type Host struct {
	cfg    HostConfig
	srv    *tailcat.Server
	ctx    context.Context
	cancel context.CancelFunc

	addr      tailcat.Addr
	invite    string
	started   time.Time
	keepName  string
	tcpByPort map[uint16]profile.Mapping
	udpByPort map[uint16]profile.Mapping

	mu       sync.Mutex
	targets  []uint16 // per group: host-side base port
	notes    []string // per group: helper notes
	scan     []portscan.Socket
	scanErr  string
	scanned  bool
	locked   bool
	admitted map[key.NodePublic]bool
	banned   map[key.NodePublic]bool
	friends  map[key.NodePublic]*friend
	active   map[string]*atomic.Int64 // "tcp/25565" -> open connections/flows
	stopped  bool
}

type friend struct {
	name, version string
	since         time.Time
	lastCtl       time.Time
	rttMs         float64
	clientPath    string
	ctl           net.Conn
	online        bool
}

// StartHost starts sharing. It returns once the tunnel server is up
// and the invite code is ready, or with an error (no internet, relay
// unreachable...).
func StartHost(ctx context.Context, cfg HostConfig) (*Host, error) {
	p := cfg.Profile
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if cfg.TargetIP == "" {
		cfg.TargetIP = "127.0.0.1"
	}
	if _, err := netip.ParseAddr(cfg.TargetIP); err != nil {
		return nil, fmt.Errorf("game server address %q must be an IP address like 127.0.0.1 or 192.168.1.20", cfg.TargetIP)
	}
	h := &Host{
		cfg:       cfg,
		tcpByPort: map[uint16]profile.Mapping{},
		udpByPort: map[uint16]profile.Mapping{},
		admitted:  map[key.NodePublic]bool{},
		banned:    map[key.NodePublic]bool{},
		friends:   map[key.NodePublic]*friend{},
		active:    map[string]*atomic.Int64{},
		targets:   make([]uint16, len(p.Ports)),
		notes:     make([]string, len(p.Ports)),
	}
	for i, pt := range p.Ports {
		h.targets[i] = pt.Port
		if t, ok := cfg.Targets[i]; ok && t != 0 {
			h.targets[i] = t
		}
	}
	var tcpPorts, udpPorts []uint16
	for _, m := range p.Mappings() {
		h.active[m.Proto+"/"+strconv.Itoa(int(m.Port))] = new(atomic.Int64)
		if m.Proto == "tcp" {
			h.tcpByPort[m.Port] = m
			tcpPorts = append(tcpPorts, m.Port)
		} else {
			h.udpByPort[m.Port] = m
			udpPorts = append(udpPorts, m.Port)
		}
	}
	tcpPorts = append(tcpPorts, profile.ControlPort)

	logf := cfg.Log.Logf
	tlogf := func(string, ...any) {}
	if cfg.Verbose {
		tlogf = func(f string, a ...any) { logf("tailcat: "+f, a...) }
	}
	srv := &tailcat.Server{
		Logf:           tlogf,
		ServedTCPPorts: portRanges(tcpPorts),
		ServedUDPPorts: portRanges(udpPorts),
		AllowClient:    h.allowClient,
		OnTCP:          h.onTCP,
		OnUDP:          h.onUDP,
	}
	if len(udpPorts) == 0 {
		srv.ServedUDPPorts = []filter.PortRange{} // non-nil: admit no UDP
	}
	var saved *config.HostKey
	if cfg.KeepCode {
		h.keepName = "game-" + p.ID
		var err error
		saved, err = config.LoadHostKey(h.keepName)
		if err != nil {
			logf("saved invite for %s is unreadable, making a new one: %v", p.Name, err)
		}
		if saved != nil {
			if err := srv.Key.UnmarshalText([]byte(saved.Key)); err != nil {
				return nil, fmt.Errorf("saved invite key: %w", err)
			}
			if err := srv.PresharedKey.UnmarshalText([]byte(saved.PSK)); err != nil {
				return nil, fmt.Errorf("saved invite key: %w", err)
			}
		} else {
			srv.Key = key.NewNode()
			srv.PresharedKey = tailcat.NewPresharedKey()
		}
	}
	h.srv = srv

	logf("Starting to share %s (ports %s)", p.Name, p.PortSummary())
	sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// Pick the relay region ourselves (rather than letting Start do it)
	// so the invite can name it by ID: that keeps invite codes short,
	// and lets a kept invite pin the same region next time.
	dm, err := tailcat.FetchDERPMap(sctx, tailcat.ExpandForServer)
	if err != nil {
		return nil, friendlyStartErr(err)
	}
	var region tailcfg.DERPRegionID
	if saved != nil && dm.Regions[tailcfg.DERPRegionID(saved.RegionID)] != nil {
		region = tailcfg.DERPRegionID(saved.RegionID)
	} else {
		region, err = tailcat.PickBestRegion(sctx, dm)
		if err != nil || dm.Regions[region] == nil {
			logf("couldn't measure relay latency (%v); using a default relay", err)
			region = 0
			for id := range dm.Regions {
				if region == 0 || id < region {
					region = id
				}
			}
		}
	}
	if r := dm.Regions[region]; r != nil {
		logf("Using relay %s (%s)", r.RegionName, r.RegionCode)
	}
	srv.RegionID = region
	if err := srv.StartContext(sctx); err != nil {
		srv.Close()
		return nil, friendlyStartErr(err)
	}
	ci, err := tailcat.ParseAddr(srv.TailcatAddr())
	if err != nil {
		srv.Close()
		return nil, err
	}
	ci.Region, ci.RegionID = nil, region
	h.addr = ci.Addr()
	if cfg.KeepCode && saved == nil {
		kb, _ := srv.Key.MarshalText()
		pb, _ := srv.PresharedKey.MarshalText()
		if err := config.SaveHostKey(h.keepName, &config.HostKey{Key: string(kb), PSK: string(pb), RegionID: int(region)}); err != nil {
			logf("couldn't save invite for next time: %v", err)
		}
	}
	code, err := invite.New(string(h.addr), cfg.HostName, p).Encode()
	if err != nil {
		srv.Close()
		return nil, err
	}
	h.invite = code
	h.started = time.Now()
	h.ctx, h.cancel = context.WithCancel(context.Background())
	go h.scanLoop()
	h.startHelpers()
	logf("Sharing %s. Invite code ready (%d characters).", p.Name, len(code))
	return h, nil
}

func friendlyStartErr(err error) error {
	s := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("couldn't reach a relay server in time; check your internet connection (%v)", err)
	case strings.Contains(s, "no such host"), strings.Contains(s, "dial tcp"):
		return fmt.Errorf("couldn't reach the internet relay servers; check your internet connection or firewall (%v)", err)
	}
	return err
}

func portRanges(ports []uint16) []filter.PortRange {
	ports = slices.Clone(ports)
	slices.Sort(ports)
	ports = slices.Compact(ports)
	var out []filter.PortRange
	for _, p := range ports {
		if n := len(out); n > 0 && out[n-1].Last+1 == p {
			out[n-1].Last = p
			continue
		}
		out = append(out, filter.PortRange{First: p, Last: p})
	}
	return out
}

func (h *Host) startHelpers() {
	known, unknown := helpers.For(h.cfg.Profile)
	for _, n := range unknown {
		h.cfg.Log.Logf("This TailLAN version doesn't have the %q helper; skipping it", n)
	}
	for _, hp := range known {
		env := &helpers.HostEnv{
			Profile:   h.cfg.Profile,
			HostName:  h.cfg.HostName,
			SetTarget: h.SetTarget,
			Logf:      h.cfg.Log.Logf,
		}
		go hp.Host(h.ctx, env)
	}
}

// SetTarget points port group g at a different host-side base port.
func (h *Host) SetTarget(g int, port uint16, note string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if g < 0 || g >= len(h.targets) || port == 0 {
		return
	}
	if h.targets[g] != port {
		h.cfg.Log.Logf("Port group %d now forwards to port %d", g+1, port)
	}
	h.targets[g] = port
	if note != "" {
		h.notes[g] = note
		h.cfg.Log.Logf("%s", note)
	}
}

// Invite returns the invite code.
func (h *Host) Invite() string { return h.invite }

// Addr returns the raw tailcat address.
func (h *Host) Addr() tailcat.Addr { return h.addr }

// Stop ends the session, telling connected friends first.
func (h *Host) Stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	var ctls []net.Conn
	for _, f := range h.friends {
		if f.ctl != nil {
			ctls = append(ctls, f.ctl)
		}
	}
	h.mu.Unlock()
	for _, c := range ctls {
		c.SetWriteDeadline(time.Now().Add(time.Second))
		writeMsg(c, ctlMsg{T: "bye", Reason: "The host stopped sharing."})
	}
	time.Sleep(150 * time.Millisecond) // let the byes flush through the tunnel
	h.cancel()
	h.srv.Close()
	h.cfg.Log.Logf("Stopped sharing %s", h.cfg.Profile.Name)
}

// SetLocked locks the room: only friends who have already connected
// (in this session) can reconnect; new invite holders are ignored.
func (h *Host) SetLocked(locked bool) {
	h.mu.Lock()
	h.locked = locked
	h.mu.Unlock()
	if locked {
		h.cfg.Log.Logf("Room locked: only friends already here can connect")
	} else {
		h.cfg.Log.Logf("Room unlocked: anyone with the invite can connect")
	}
}

// Kick disconnects a friend and keeps them out for the rest of this
// session. id is the friend's ID from the status.
func (h *Host) Kick(id string) error {
	var k key.NodePublic
	if err := k.UnmarshalText([]byte(id)); err != nil {
		return fmt.Errorf("bad friend id: %w", err)
	}
	h.mu.Lock()
	h.banned[k] = true
	f := h.friends[k]
	var ctl net.Conn
	name := shortKey(k)
	if f != nil {
		ctl = f.ctl
		f.online = false
		if f.name != "" {
			name = f.name
		}
	}
	h.mu.Unlock()
	if ctl != nil {
		ctl.SetWriteDeadline(time.Now().Add(time.Second))
		writeMsg(ctl, ctlMsg{T: "bye", Reason: "The host removed you from the game."})
		ctl.Close()
	}
	h.srv.DisconnectClient(k)
	h.cfg.Log.Logf("Removed %s from the session", name)
	return nil
}

func (h *Host) allowClient(k key.NodePublic) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.banned[k] {
		return false
	}
	if h.locked && !h.admitted[k] {
		return false
	}
	if !h.admitted[k] {
		h.admitted[k] = true
		h.cfg.Log.Logf("A friend is connecting (%s)", shortKey(k))
	}
	return true
}

// dialTarget picks the address to reach the game on: where the scan
// saw it bound if that's a specific address, else TargetIP.
func (h *Host) dialTargets(proto string, port uint16) []string {
	h.mu.Lock()
	scan := h.scan
	h.mu.Unlock()
	ps := strconv.Itoa(int(port))
	var out []string
	if h.cfg.TargetIP == "127.0.0.1" {
		for _, s := range portscan.Find(scan, proto, port) {
			if !s.Loopback() {
				out = append(out, net.JoinHostPort(s.IP, ps)) // bound to one LAN IP only
			} else if s.IP == "::1" {
				out = append(out, net.JoinHostPort("::1", ps))
			}
		}
		out = append(out, net.JoinHostPort("127.0.0.1", ps))
		if proto == "tcp" {
			out = append(out, net.JoinHostPort("::1", ps))
		}
	} else {
		out = append(out, net.JoinHostPort(h.cfg.TargetIP, ps))
	}
	return slices.Compact(out)
}

func (h *Host) targetPort(m profile.Mapping) uint16 {
	h.mu.Lock()
	defer h.mu.Unlock()
	base := h.cfg.Profile.Ports[m.Group].Port
	return h.targets[m.Group] + (m.Port - base)
}

func (h *Host) onTCP(port uint16) func(net.Conn) {
	if port == profile.ControlPort {
		return h.handleControl
	}
	m, ok := h.tcpByPort[port]
	if !ok {
		return nil
	}
	counter := h.active["tcp/"+strconv.Itoa(int(port))]
	return func(c net.Conn) {
		tp := h.targetPort(m)
		var local net.Conn
		var err error
		for _, a := range h.dialTargets("tcp", tp) {
			local, err = net.DialTimeout("tcp", a, 3*time.Second)
			if err == nil {
				break
			}
		}
		if err != nil {
			h.cfg.Log.Logf("A friend tried to connect on TCP %d, but nothing answered on port %d. Is the game server running?", port, tp)
			c.Close()
			return
		}
		counter.Add(1)
		defer counter.Add(-1)
		tailcat.ProxyConns(c, local)
	}
}

func (h *Host) onUDP(port uint16) func(tailcat.ConnPacketConn) {
	m, ok := h.udpByPort[port]
	if !ok {
		return nil
	}
	counter := h.active["udp/"+strconv.Itoa(int(port))]
	return func(c tailcat.ConnPacketConn) {
		tp := h.targetPort(m)
		dst := h.dialTargets("udp", tp)[0]
		ua, err := net.ResolveUDPAddr("udp", dst)
		var local *net.UDPConn
		if err == nil {
			local, err = net.DialUDP("udp", nil, ua)
		}
		if err != nil {
			h.cfg.Log.Logf("can't forward UDP %d to %s: %v", port, dst, err)
			c.Close()
			return
		}
		counter.Add(1)
		defer counter.Add(-1)
		tailcat.ProxyPacketConns(c, local)
	}
}

func (h *Host) handleControl(c net.Conn) {
	defer c.Close()
	k, ok := h.srv.PeerKey(c.RemoteAddr())
	if !ok {
		return
	}
	r := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var hello ctlMsg
	if json.Unmarshal(line, &hello) != nil || hello.T != "hello" {
		return
	}
	name := strings.TrimSpace(hello.Name)
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40])
	}
	h.mu.Lock()
	if h.banned[k] || h.stopped {
		h.mu.Unlock()
		return
	}
	f := h.friends[k]
	if f == nil {
		f = &friend{since: time.Now()}
		h.friends[k] = f
	}
	if f.ctl != nil {
		f.ctl.Close() // a reconnect replaces the old channel
	}
	f.name, f.version, f.ctl, f.online, f.lastCtl = name, hello.Version, c, true, time.Now()
	h.mu.Unlock()
	h.cfg.Log.Logf("%s joined", displayName(name, k))
	writeMsg(c, ctlMsg{T: "welcome", Name: h.cfg.HostName, Version: Version, Game: h.cfg.Profile.Name})

	defer func() {
		h.mu.Lock()
		if f.ctl == c {
			f.ctl, f.online = nil, false
			h.cfg.Log.Logf("%s left", displayName(f.name, k))
		}
		h.mu.Unlock()
	}()
	for {
		c.SetReadDeadline(time.Now().Add(20 * time.Second))
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var m ctlMsg
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch m.T {
		case "ping":
			h.mu.Lock()
			f.lastCtl, f.rttMs, f.clientPath = time.Now(), m.RTT, m.Path
			h.mu.Unlock()
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if writeMsg(c, ctlMsg{T: "pong", N: m.N}) != nil {
				return
			}
		case "bye":
			return
		}
	}
}

func displayName(name string, k key.NodePublic) string {
	if name != "" {
		return name
	}
	return "Friend " + shortKey(k)
}

// shortKey is a friendly fingerprint for a friend's key: "f4cm".
func shortKey(k key.NodePublic) string {
	s := strings.Trim(k.ShortString(), "[]/")
	if len(s) > 4 {
		s = s[:4]
	}
	return s
}

// scanLoop keeps the "is the game server up?" checks fresh.
func (h *Host) scanLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		socks, err := portscan.Listening(ctx)
		cancel()
		h.mu.Lock()
		h.scanned = true
		if err != nil {
			h.scanErr = err.Error()
		} else {
			h.scan, h.scanErr = socks, ""
		}
		h.mu.Unlock()
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// HostStatus is the host session's state for the UI.
type HostStatus struct {
	Role      string           `json:"role"`
	Game      *profile.Profile `json:"game"`
	Invite    string           `json:"invite"`
	Addr      string           `json:"addr"`
	HostName  string           `json:"hostName"`
	KeepCode  bool             `json:"keepCode"`
	Locked    bool             `json:"locked"`
	TargetIP  string           `json:"targetIP"`
	Started   time.Time        `json:"started"`
	Groups    []GroupStatus    `json:"groups"`
	Friends   []FriendStatus   `json:"friends"`
	ScanError string           `json:"scanError,omitempty"`
	// TailcatHint is a command for friends without TailLAN (TCP-only
	// games), using the plain tailcat CLI.
	TailcatHint string `json:"tailcatHint,omitempty"`
}

// GroupStatus is one profile port entry on the host.
type GroupStatus struct {
	Index     int    `json:"index"`
	Label     string `json:"label"`
	Proto     string `json:"proto"`
	Ports     string `json:"ports"`  // tunnel ports, "2456-2457"
	Target    uint16 `json:"target"` // host-side base port
	Ask       string `json:"ask,omitempty"`
	Listening string `json:"listening"` // "yes", "no", "partial", "unknown"
	Process   string `json:"process,omitempty"`
	Note      string `json:"note,omitempty"`
	Active    int64  `json:"active"` // open connections / flows
}

// FriendStatus is one connected (or recently connected) friend.
type FriendStatus struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	App      string   `json:"app"` // "TailLAN" or "tailcat"
	Online   bool     `json:"online"`
	Path     PathInfo `json:"path"`
	Since    string   `json:"since,omitempty"`
	RxBytes  int64    `json:"rxBytes"`
	TxBytes  int64    `json:"txBytes"`
	Version  string   `json:"version,omitempty"`
	Kickable bool     `json:"kickable"`
}

// Status snapshots the session.
func (h *Host) Status() *HostStatus {
	p := h.cfg.Profile
	st := h.srv.Status()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := &HostStatus{
		Role:     "host",
		Game:     p,
		Invite:   h.invite,
		Addr:     string(h.addr),
		HostName: h.cfg.HostName,
		KeepCode: h.cfg.KeepCode,
		Locked:   h.locked,
		TargetIP: h.cfg.TargetIP,
		Started:  h.started,
		Friends:  []FriendStatus{},
	}
	if h.scanErr != "" {
		out.ScanError = h.scanErr
	}
	var tcpPorts []string
	allTCP := true
	for i, pt := range p.Ports {
		g := GroupStatus{
			Index:  i,
			Label:  pt.Label,
			Proto:  pt.Proto,
			Ports:  fmtRange(pt.Port, groupLast(pt)),
			Target: h.targets[i],
			Ask:    pt.Ask,
			Note:   h.notes[i],
		}
		if pt.Proto == "tcp" {
			for port := int(pt.Port); port <= int(groupLast(pt)); port++ {
				tcpPorts = append(tcpPorts, strconv.Itoa(port))
			}
		} else {
			allTCP = false
		}
		found, want := 0, 0
		var procs []string
		for _, proto := range protosOf(pt) {
			for off := 0; off <= int(groupLast(pt)-pt.Port); off++ {
				want++
				tp := g.Target + uint16(off)
				socks := portscan.Find(h.scan, proto, tp)
				if len(socks) > 0 {
					found++
					if n := socks[0].Process; n != "" && !slices.Contains(procs, n) {
						procs = append(procs, n)
					}
				}
				if c := h.active[proto+"/"+strconv.Itoa(int(pt.Port)+off)]; c != nil {
					g.Active += c.Load()
				}
			}
		}
		switch {
		case h.cfg.TargetIP != "127.0.0.1":
			g.Listening = "unknown" // on another device; can't see its sockets
		case !h.scanned || h.scanErr != "":
			g.Listening = "unknown"
		case found == want:
			g.Listening = "yes"
		case found > 0:
			g.Listening = "partial"
		default:
			g.Listening = "no"
		}
		g.Process = strings.Join(procs, ", ")
		out.Groups = append(out.Groups, g)
	}
	if allTCP && len(tcpPorts) > 0 {
		out.TailcatHint = "tailcat forward " + string(h.addr) + " " + strings.Join(tcpPorts, " ")
	}

	seen := map[key.NodePublic]bool{}
	if st != nil {
		for k, ps := range st.Peer {
			seen[k] = true
			f := h.friends[k]
			fs := FriendStatus{
				ID:       k.String(),
				Name:     displayName("", k),
				App:      "tailcat",
				RxBytes:  ps.RxBytes,
				TxBytes:  ps.TxBytes,
				Kickable: true,
			}
			recent := !ps.LastHandshake.IsZero() && time.Since(ps.LastHandshake) < 3*time.Minute
			fs.Online = recent
			if ps.CurAddr != "" {
				fs.Path = PathInfo{Kind: "direct", Via: ps.CurAddr}
			} else if ps.Relay != "" {
				fs.Path = PathInfo{Kind: "relay", Via: ps.Relay}
			}
			if f != nil {
				fs.Name = displayName(f.name, k)
				fs.App = "TailLAN"
				fs.Online = f.online
				fs.Version = f.version
				fs.Since = f.since.Format(time.RFC3339)
				fs.Path.LatencyMs = f.rttMs
			}
			if h.banned[k] {
				continue
			}
			out.Friends = append(out.Friends, fs)
		}
	}
	for k, f := range h.friends {
		if seen[k] || h.banned[k] || !f.online {
			continue
		}
		out.Friends = append(out.Friends, FriendStatus{
			ID: k.String(), Name: displayName(f.name, k), App: "TailLAN", Online: true,
			Version: f.version, Since: f.since.Format(time.RFC3339), Kickable: true,
			Path: PathInfo{LatencyMs: f.rttMs},
		})
	}
	slices.SortFunc(out.Friends, func(a, b FriendStatus) int {
		if a.Online != b.Online {
			if a.Online {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}
