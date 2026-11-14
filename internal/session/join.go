package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"github.com/LaneBirmingham/taillan/internal/helpers"
	"github.com/LaneBirmingham/taillan/internal/invite"
	"github.com/LaneBirmingham/taillan/internal/portscan"
	"github.com/LaneBirmingham/taillan/internal/profile"
)

// JoinProfile combines an invite's game info with the local built-in
// profile of the same game, if this TailLAN has one: ports and the
// connect template always come from the invite (the host may have
// customised them), while instructions, notes, links and helpers come
// from the trusted local copy.
func JoinProfile(inv *invite.Invite, local *profile.Profile) (*profile.Profile, error) {
	p, err := inv.Profile()
	if err != nil {
		return nil, err
	}
	// Fill in what the invite left out (built-in games travel without
	// their steps) from this player's own copy of the game. Whatever
	// the host sent wins: it reflects the host's version of the game.
	if local != nil {
		if len(p.Join.Steps) == 0 {
			p.Join = local.Join
		}
		if p.Notes == "" {
			p.Notes = local.Notes
		}
		if len(p.Helpers) == 0 {
			p.Helpers = local.Helpers
		}
		if p.Connect == "" {
			p.Connect = local.Connect
		}
		p.Links = local.Links
		p.Tags = local.Tags
	}
	if len(p.Join.Steps) == 0 {
		p.Join.Steps = []string{"In the game, connect to {{connect}}"}
	}
	return p, nil
}

// JoinConfig configures joining a friend's game.
type JoinConfig struct {
	Invite  *invite.Invite
	Profile *profile.Profile // from JoinProfile
	Name    string           // our display name, shown to the host
	Key     key.NodePrivate  // our identity; zero for a fresh one
	// AllowLAN lets other devices on this player's LAN use the
	// forwarded ports marked "lan" (a console or second PC in the same
	// house). Off by default: only this computer may use them.
	AllowLAN bool
	Verbose  bool
	Log      *LogBuffer
}

// Join is a running session connected (or connecting) to a host.
type Join struct {
	cfg    JoinConfig
	cl     *tailcat.Client
	ctx    context.Context
	cancel context.CancelFunc

	groupLocal []uint16 // per group: local base port
	tcp        []*tcpFwd
	udp        []*udpFwd
	started    time.Time

	mu          sync.Mutex
	phase       string // connecting, connected, reconnecting, ended
	message     string
	path        PathInfo
	rttMs       float64
	hostVersion string
	notes       []string
	everUp      bool
	stopped     bool
	ctl         net.Conn // current control connection, if any

	localIPs atomic.Pointer[map[string]bool]
}

// StartJoin opens the local ports and starts connecting in the
// background. It fails only for local problems (a port that must be
// free isn't); network trouble shows up in Status while it retries.
func StartJoin(cfg JoinConfig) (*Join, error) {
	p := cfg.Profile
	j := &Join{cfg: cfg, phase: "connecting", started: time.Now()}
	j.ctx, j.cancel = context.WithCancel(context.Background())
	j.refreshLocalIPs()

	logf := cfg.Log.Logf
	tlogf := func(string, ...any) {}
	if cfg.Verbose {
		tlogf = func(f string, a ...any) { logf("tailcat: "+f, a...) }
	}
	j.cl = &tailcat.Client{Server: tailcat.Addr(cfg.Invite.Addr), Key: cfg.Key, Logf: tlogf}

	if err := j.openPorts(); err != nil {
		j.closePorts()
		j.cancel()
		return nil, err
	}
	var desc []string
	for g, pt := range p.Ports {
		desc = append(desc, fmt.Sprintf("%s/%s", fmtRange(j.groupLocal[g], j.groupLocal[g]+(groupLast(pt)-pt.Port)), pt.Proto))
	}
	logf("Joining %s's %s: forwarding local ports %s", hostLabel(cfg.Invite.HostName), p.Name, strings.Join(desc, ", "))
	go j.connectLoop()
	return j, nil
}

func hostLabel(name string) string {
	if name == "" {
		return "your friend"
	}
	return name
}

// openPorts binds every local listener, shifting a port group to free
// ports if the defaults are taken (unless the game needs exact ports).
func (j *Join) openPorts() error {
	p := j.cfg.Profile
	j.groupLocal = make([]uint16, len(p.Ports))
	deltas := []int{0, 1, 2, 3, 4, 5, 10, 20, 50, 100, 1000, 2000, 3000}
	for g, pt := range p.Ports {
		bind := "127.0.0.1"
		if pt.LAN {
			bind = "0.0.0.0"
		}
		span := int(groupLast(pt) - pt.Port)
		var lastErr error
		ok := false
		for _, d := range deltas {
			if pt.SamePort && d != 0 {
				break
			}
			base := int(pt.Port) + d
			if base+span > 65535 {
				continue
			}
			tcps, udps, err := j.bindGroup(g, pt, bind, uint16(base))
			if err != nil {
				lastErr = err
				continue
			}
			j.tcp = append(j.tcp, tcps...)
			j.udp = append(j.udp, udps...)
			j.groupLocal[g] = uint16(base)
			ok = true
			if d != 0 {
				j.cfg.Log.Logf("Port %d is busy on this computer, so %s uses %d instead", pt.Port, labelOr(pt.Label, "the game"), base)
			}
			break
		}
		if !ok {
			return busyErr(pt, lastErr)
		}
	}
	return nil
}

func labelOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func busyErr(pt profile.Port, err error) error {
	who := ""
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if socks, e := portscan.Listening(ctx); e == nil {
		for _, proto := range protosOf(pt) {
			for _, s := range portscan.Find(socks, proto, pt.Port) {
				if s.Process != "" {
					who = " by " + s.Process
				}
			}
		}
	}
	if pt.SamePort {
		return fmt.Errorf("port %d/%s is already in use on this computer%s, and this game needs exactly that port. Close the program using it (are you running the game's server yourself?) and try again", pt.Port, pt.Proto, who)
	}
	return fmt.Errorf("couldn't open port %d/%s or any nearby port on this computer%s: %v", pt.Port, pt.Proto, who, err)
}

func (j *Join) bindGroup(g int, pt profile.Port, bind string, base uint16) ([]*tcpFwd, []*udpFwd, error) {
	var tcps []*tcpFwd
	var udps []*udpFwd
	fail := func(err error) ([]*tcpFwd, []*udpFwd, error) {
		for _, t := range tcps {
			t.ln.Close()
		}
		for _, u := range udps {
			u.pc.Close()
		}
		return nil, nil, err
	}
	for off := 0; off <= int(groupLast(pt)-pt.Port); off++ {
		local := base + uint16(off)
		remote := pt.Port + uint16(off)
		addr := net.JoinHostPort(bind, strconv.Itoa(int(local)))
		for _, proto := range protosOf(pt) {
			if proto == "tcp" {
				ln, err := net.Listen("tcp4", addr)
				if err != nil {
					return fail(err)
				}
				tcps = append(tcps, &tcpFwd{j: j, ln: ln, group: g, local: local, remote: remote})
			} else {
				ua, _ := net.ResolveUDPAddr("udp4", addr)
				pc, err := net.ListenUDP("udp4", ua)
				if err != nil {
					return fail(err)
				}
				udps = append(udps, &udpFwd{j: j, pc: pc, group: g, local: local, remote: remote, flows: map[string]*udpFlow{}})
			}
		}
	}
	return tcps, udps, nil
}

func (j *Join) closePorts() {
	for _, t := range j.tcp {
		t.ln.Close()
	}
	for _, u := range j.udp {
		u.close()
	}
}

func (j *Join) refreshLocalIPs() {
	m := map[string]bool{"127.0.0.1": true, "::1": true}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			m[ipn.IP.String()] = true
		}
	}
	j.localIPs.Store(&m)
}

// allowed reports whether a local connection may use a forward: one
// from this computer, or from the LAN if the player allowed that.
func (j *Join) allowed(ip net.IP) bool {
	if j.cfg.AllowLAN || ip.IsLoopback() {
		return true
	}
	if (*j.localIPs.Load())[ip.String()] {
		return true
	}
	j.refreshLocalIPs()
	return (*j.localIPs.Load())[ip.String()]
}

func (j *Join) setPhase(phase, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.phase == "ended" {
		return
	}
	if j.phase != phase || j.message != msg {
		if msg != "" {
			j.cfg.Log.Logf("%s", msg)
		}
	}
	j.phase, j.message = phase, msg
}

func (j *Join) connectLoop() {
	host := hostLabel(j.cfg.Invite.HostName)
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(j.ctx, 25*time.Second)
		_, err := j.cl.Ping(ctx)
		cancel()
		if err == nil {
			break
		}
		if j.ctx.Err() != nil {
			return
		}
		msg := fmt.Sprintf("Can't reach %s yet. Check they're still sharing (invites stop working when the host stops). Retrying...", host)
		if strings.Contains(err.Error(), "DERP") || strings.Contains(err.Error(), "dial") {
			msg = "Can't reach the relay server. Check your internet connection. Retrying..."
		}
		j.setPhase("connecting", msg)
		j.cfg.Log.Logf("connect attempt %d: %v", attempt+1, err)
		select {
		case <-j.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	j.mu.Lock()
	j.everUp = true
	j.mu.Unlock()
	j.setPhase("connected", fmt.Sprintf("Connected to %s", host))
	for _, t := range j.tcp {
		go t.serve()
	}
	for _, u := range j.udp {
		go u.serve()
	}
	go j.pathLoop()
	go j.controlLoop()
	j.startHelpers()
}

func (j *Join) startHelpers() {
	known, unknown := helpers.For(j.cfg.Profile)
	for _, n := range unknown {
		j.cfg.Log.Logf("This TailLAN version doesn't have the %q helper; skipping it", n)
	}
	for _, hp := range known {
		env := &helpers.JoinEnv{
			Profile:  j.cfg.Profile,
			HostName: j.cfg.Invite.HostName,
			LocalPort: func(g int) uint16 {
				if g < 0 || g >= len(j.groupLocal) {
					return 0
				}
				return j.groupLocal[g]
			},
			Note: func(n string) {
				j.mu.Lock()
				defer j.mu.Unlock()
				for _, have := range j.notes {
					if have == n {
						return
					}
				}
				j.notes = append(j.notes, n)
			},
			Logf: j.cfg.Log.Logf,
		}
		go hp.Join(j.ctx, env)
	}
}

func (j *Join) pathLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(j.ctx, 3*time.Second)
		r, err := j.cl.DiscoPing(ctx)
		cancel()
		if err == nil && r != nil {
			pi := PathInfo{LatencyMs: r.LatencySeconds * 1000}
			switch {
			case r.Endpoint != "":
				pi.Kind, pi.Via = "direct", r.Endpoint
			case r.DERPRegionCode != "":
				pi.Kind, pi.Via = "relay", r.DERPRegionCode
			}
			j.mu.Lock()
			if j.path.Kind != pi.Kind && pi.Kind != "" {
				if pi.Kind == "direct" {
					j.cfg.Log.Logf("Direct connection to host established (%s)", pi.Via)
				} else {
					j.cfg.Log.Logf("Traffic is relayed via %s (direct connection not possible yet)", pi.Via)
				}
			}
			j.path = pi
			j.mu.Unlock()
		}
		select {
		case <-j.ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (j *Join) controlLoop() {
	host := hostLabel(j.cfg.Invite.HostName)
	for j.ctx.Err() == nil {
		ended := j.controlOnce()
		if ended || j.ctx.Err() != nil {
			return
		}
		j.setPhase("reconnecting", fmt.Sprintf("Lost contact with %s. Reconnecting...", host))
		select {
		case <-j.ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// controlOnce runs one control connection; it reports whether the host
// ended the session.
func (j *Join) controlOnce() (ended bool) {
	ctx, cancel := context.WithTimeout(j.ctx, 15*time.Second)
	c, err := j.cl.DialTCPPort(ctx, profile.ControlPort)
	cancel()
	if err != nil {
		return false
	}
	defer c.Close()
	j.mu.Lock()
	j.ctl = c
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		if j.ctl == c {
			j.ctl = nil
		}
		j.mu.Unlock()
	}()
	if writeMsg(c, ctlMsg{T: "hello", Name: j.cfg.Name, Version: Version}) != nil {
		return false
	}
	var (
		mu      sync.Mutex
		sent    = map[int64]time.Time{}
		lastRx  = time.Now()
		byeWhy  string
		readErr = make(chan struct{})
	)
	go func() {
		defer close(readErr)
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var m ctlMsg
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			mu.Lock()
			lastRx = time.Now()
			mu.Unlock()
			switch m.T {
			case "welcome":
				j.mu.Lock()
				j.hostVersion = m.Version
				j.mu.Unlock()
				j.setPhase("connected", fmt.Sprintf("Connected to %s", hostLabel(j.cfg.Invite.HostName)))
			case "pong":
				mu.Lock()
				if t0, ok := sent[m.N]; ok {
					j.mu.Lock()
					j.rttMs = float64(time.Since(t0).Microseconds()) / 1000
					j.mu.Unlock()
					delete(sent, m.N)
				}
				mu.Unlock()
			case "bye":
				mu.Lock()
				byeWhy = m.Reason
				mu.Unlock()
				return
			}
		}
	}()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var n int64
	for {
		select {
		case <-readErr:
			mu.Lock()
			why := byeWhy
			mu.Unlock()
			if why != "" {
				j.end(why)
				return true
			}
			return false
		case <-t.C:
			mu.Lock()
			stale := time.Since(lastRx) > 12*time.Second
			n++
			sent[n] = time.Now()
			for k := range sent {
				if k < n-10 {
					delete(sent, k)
				}
			}
			mu.Unlock()
			if stale {
				return false
			}
			j.mu.Lock()
			msg := ctlMsg{T: "ping", N: n, RTT: j.rttMs, Path: j.path.Kind}
			j.mu.Unlock()
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if writeMsg(c, msg) != nil {
				return false
			}
		}
	}
}

// end stops forwarding because the host ended the session.
func (j *Join) end(reason string) {
	j.mu.Lock()
	j.phase, j.message = "ended", reason
	j.mu.Unlock()
	j.cfg.Log.Logf("Session ended: %s", reason)
	j.shutdown()
}

// Stop leaves the game.
func (j *Join) Stop() {
	j.mu.Lock()
	if j.phase != "ended" {
		j.phase, j.message = "ended", "You left the game."
	}
	ctl := j.ctl
	j.mu.Unlock()
	if ctl != nil {
		// Tell the host right away rather than letting them time out.
		ctl.SetWriteDeadline(time.Now().Add(time.Second))
		writeMsg(ctl, ctlMsg{T: "bye"})
		time.Sleep(100 * time.Millisecond)
	}
	j.shutdown()
	j.cfg.Log.Logf("Left %s's game", hostLabel(j.cfg.Invite.HostName))
}

func (j *Join) shutdown() {
	j.mu.Lock()
	if j.stopped {
		j.mu.Unlock()
		return
	}
	j.stopped = true
	j.mu.Unlock()
	j.cancel()
	j.closePorts()
	j.cl.Close()
}

// Ended reports whether the session is over.
func (j *Join) Ended() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.phase == "ended"
}

// JoinStatus is the join session's state for the UI.
type JoinStatus struct {
	Role        string           `json:"role"`
	Phase       string           `json:"phase"`
	Message     string           `json:"message,omitempty"`
	HostName    string           `json:"hostName"`
	HostVersion string           `json:"hostVersion,omitempty"`
	Game        *profile.Profile `json:"game"`
	Connect     string           `json:"connect"`
	Steps       []string         `json:"steps"`
	Path        PathInfo         `json:"path"`
	RTTMs       float64          `json:"rttMs,omitempty"`
	Ports       []JoinPort       `json:"ports"`
	Notes       []string         `json:"notes,omitempty"`
	Started     time.Time        `json:"started"`
	Warnings    []string         `json:"warnings,omitempty"`
}

// JoinPort is one forwarded port group on the joiner's machine.
type JoinPort struct {
	Label    string `json:"label,omitempty"`
	Proto    string `json:"proto"`
	Local    string `json:"local"`  // "127.0.0.1:2456-2457"
	Remote   string `json:"remote"` // "2456-2457"
	Active   int64  `json:"active"`
	Oversize int64  `json:"oversize,omitempty"`
}

// Status snapshots the session.
func (j *Join) Status() *JoinStatus {
	p := j.cfg.Profile
	vars := joinVars(p, j.cfg.Invite.HostName, j.groupLocal)
	j.mu.Lock()
	st := &JoinStatus{
		Role:        "join",
		Phase:       j.phase,
		Message:     j.message,
		HostName:    j.cfg.Invite.HostName,
		HostVersion: j.hostVersion,
		Game:        p,
		Connect:     vars["connect"],
		Path:        j.path,
		RTTMs:       j.rttMs,
		Notes:       append([]string{}, j.notes...),
		Steps:       []string{},
		Started:     j.started,
	}
	j.mu.Unlock()
	for _, s := range p.Join.Steps {
		st.Steps = append(st.Steps, Render(s, vars))
	}
	var oversize int64
	for g, pt := range p.Ports {
		span := groupLast(pt) - pt.Port
		bind := "127.0.0.1"
		if pt.LAN {
			bind = "this computer (all interfaces)"
		}
		jp := JoinPort{
			Label:  pt.Label,
			Proto:  pt.Proto,
			Local:  bind + ":" + fmtRange(j.groupLocal[g], j.groupLocal[g]+span),
			Remote: fmtRange(pt.Port, groupLast(pt)),
		}
		if !pt.LAN {
			jp.Local = "127.0.0.1:" + fmtRange(j.groupLocal[g], j.groupLocal[g]+span)
		}
		for _, t := range j.tcp {
			if t.group == g {
				jp.Active += t.active.Load()
			}
		}
		for _, u := range j.udp {
			if u.group == g {
				jp.Active += u.active.Load()
				jp.Oversize += u.oversize.Load()
			}
		}
		oversize += jp.Oversize
		st.Ports = append(st.Ports, jp)
	}
	if oversize > 0 {
		st.Warnings = append(st.Warnings, fmt.Sprintf("The game sent %d UDP packets larger than the tunnel's %d-byte limit; some may be lost. If you see glitches, this game may not work well over TailLAN.", oversize, tailcat.MaxUDPPayload))
	}
	return st
}

// tcpFwd forwards one local TCP port into the tunnel.
type tcpFwd struct {
	j             *Join
	ln            net.Listener
	group         int
	local, remote uint16
	active        atomic.Int64
}

func (t *tcpFwd) serve() {
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return
		}
		if ta, ok := c.RemoteAddr().(*net.TCPAddr); !ok || !t.j.allowed(ta.IP) {
			c.Close()
			continue
		}
		go func() {
			ctx, cancel := context.WithTimeout(t.j.ctx, 20*time.Second)
			tc, err := t.j.cl.DialTCPPort(ctx, t.remote)
			cancel()
			if err != nil {
				t.j.cfg.Log.Logf("couldn't open TCP %d to host: %v", t.remote, err)
				c.Close()
				return
			}
			t.active.Add(1)
			defer t.active.Add(-1)
			tailcat.ProxyConns(c, tc)
		}()
	}
}

// udpFwd forwards one local UDP port into the tunnel, with one tunnel
// flow per local source address (so several game clients on one
// machine, or a client that rebinds, each get their own flow).
type udpFwd struct {
	j             *Join
	pc            *net.UDPConn
	group         int
	local, remote uint16
	active        atomic.Int64
	oversize      atomic.Int64

	mu    sync.Mutex
	flows map[string]*udpFlow
}

type udpFlow struct {
	src  *net.UDPAddr
	ch   chan []byte
	last atomic.Int64 // unix nanos of last activity
}

const udpIdle = 2 * time.Minute

func (u *udpFwd) serve() {
	buf := make([]byte, 65535)
	for {
		n, src, err := u.pc.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. Windows ICMP "connection reset" on UDP
		}
		if !u.j.allowed(src.IP) {
			continue
		}
		if n > tailcat.MaxUDPPayload {
			u.oversize.Add(1)
		}
		k := src.String()
		u.mu.Lock()
		f := u.flows[k]
		if f == nil {
			f = &udpFlow{src: src, ch: make(chan []byte, 256)}
			u.flows[k] = f
			go u.runFlow(k, f)
		}
		u.mu.Unlock()
		pkt := append([]byte(nil), buf[:n]...)
		select {
		case f.ch <- pkt:
		default: // flow backed up (or still dialing); drop like a congested network would
		}
	}
}

func (u *udpFwd) runFlow(k string, f *udpFlow) {
	defer func() {
		u.mu.Lock()
		delete(u.flows, k)
		u.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(u.j.ctx, 30*time.Second)
	tc, err := u.j.cl.DialUDPPort(ctx, u.remote)
	cancel()
	if err != nil {
		u.j.cfg.Log.Logf("couldn't open UDP %d to host: %v", u.remote, err)
		return
	}
	u.active.Add(1)
	defer u.active.Add(-1)
	defer tc.Close()
	f.last.Store(time.Now().UnixNano())
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := tc.Read(buf)
			if err != nil {
				return
			}
			f.last.Store(time.Now().UnixNano())
			u.pc.WriteToUDP(buf[:n], f.src)
		}
	}()
	idle := time.NewTicker(15 * time.Second)
	defer idle.Stop()
	for {
		select {
		case <-u.j.ctx.Done():
			return
		case pkt := <-f.ch:
			f.last.Store(time.Now().UnixNano())
			if _, err := tc.Write(pkt); err != nil {
				return
			}
		case <-idle.C:
			if time.Since(time.Unix(0, f.last.Load())) > udpIdle {
				return
			}
		}
	}
}

func (u *udpFwd) close() {
	u.pc.Close()
}
