// Package portscan answers "what is listening on this machine?" It
// powers two tools: the host's live "is the game server up?" checks,
// and the port detector that figures out which ports an unknown game
// uses so it can be saved as a profile.
package portscan

import (
	"context"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	psnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// Socket is one listening TCP socket or bound UDP socket.
type Socket struct {
	Proto   string `json:"proto"` // "tcp" or "udp"
	IP      string `json:"ip"`    // bound address; "0.0.0.0"/"::" mean all
	Port    uint16 `json:"port"`
	PID     int32  `json:"pid"`
	Process string `json:"process"`
	Self    bool   `json:"self,omitempty"` // TailLAN's own socket
}

// Loopback reports whether a connection to 127.0.0.1 reaches s.
func (s Socket) Loopback() bool {
	switch s.IP {
	case "", "0.0.0.0", "::", "*", "127.0.0.1", "::1":
		return true
	}
	a, err := netip.ParseAddr(s.IP)
	return err == nil && a.IsLoopback()
}

// Listening lists listening sockets with their owning processes.
// Some processes may be unnamed if the OS won't say (other users'
// processes, on some systems).
func Listening(ctx context.Context) ([]Socket, error) {
	conns, err := psnet.ConnectionsWithoutUidsWithContext(ctx, "inet")
	if err != nil {
		return nil, err
	}
	self := int32(os.Getpid())
	var out []Socket
	seen := map[string]bool{}
	for _, c := range conns {
		var s Socket
		switch c.Type {
		case 1: // SOCK_STREAM
			if c.Status != "LISTEN" {
				continue
			}
			s.Proto = "tcp"
		case 2: // SOCK_DGRAM
			if c.Raddr.Port != 0 {
				continue // connected client socket, not a server
			}
			s.Proto = "udp"
		default:
			continue
		}
		if c.Laddr.Port == 0 || c.Laddr.Port > 65535 {
			continue
		}
		s.IP, s.Port, s.PID = c.Laddr.IP, uint16(c.Laddr.Port), c.Pid
		s.Self = c.Pid == self
		k := s.Proto + "|" + s.IP + "|" + strconv.Itoa(int(s.Port)) + "|" + strconv.Itoa(int(s.PID))
		if seen[k] {
			continue
		}
		seen[k] = true
		s.Process = processName(ctx, c.Pid)
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Socket) int {
		if a.Port != b.Port {
			return int(a.Port) - int(b.Port)
		}
		return strings.Compare(a.Proto, b.Proto)
	})
	return out, nil
}

// Find returns the sockets listening on port/proto.
func Find(socks []Socket, proto string, port uint16) []Socket {
	var out []Socket
	for _, s := range socks {
		if s.Proto == proto && s.Port == port {
			out = append(out, s)
		}
	}
	return out
}

// Process groups the sockets of one process.
type Process struct {
	PID     int32    `json:"pid"`
	Name    string   `json:"name"`
	Self    bool     `json:"self,omitempty"`
	Sockets []Socket `json:"sockets"`
	// Spec is the sockets as a port spec ("7777/udp, 27015/tcp"),
	// ready to paste into a custom game.
	Spec string `json:"spec"`
}

// ByProcess groups sockets per process, game-looking processes first
// (those with fewer, non-system ports), sorted by name.
func ByProcess(socks []Socket) []Process {
	m := map[int32]*Process{}
	var order []int32
	for _, s := range socks {
		p := m[s.PID]
		if p == nil {
			p = &Process{PID: s.PID, Name: s.Process, Self: s.Self}
			m[s.PID] = p
			order = append(order, s.PID)
		}
		p.Sockets = append(p.Sockets, s)
	}
	out := make([]Process, 0, len(order))
	for _, pid := range order {
		p := m[pid]
		p.Spec = Spec(p.Sockets)
		if p.Name == "" {
			p.Name = "PID " + strconv.Itoa(int(pid))
			if pid <= 0 {
				p.Name = "System / other users"
			}
		}
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b Process) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

// Spec renders sockets as a deduplicated port spec, merging a port
// that is both TCP and UDP into "N/both".
func Spec(socks []Socket) string {
	protos := map[uint16]map[string]bool{}
	var ports []uint16
	for _, s := range socks {
		if protos[s.Port] == nil {
			protos[s.Port] = map[string]bool{}
			ports = append(ports, s.Port)
		}
		protos[s.Port][s.Proto] = true
	}
	slices.Sort(ports)
	var parts []string
	for _, p := range ports {
		pr := protos[p]
		switch {
		case pr["tcp"] && pr["udp"]:
			parts = append(parts, strconv.Itoa(int(p))+"/both")
		case pr["tcp"]:
			parts = append(parts, strconv.Itoa(int(p))+"/tcp")
		default:
			parts = append(parts, strconv.Itoa(int(p))+"/udp")
		}
	}
	return strings.Join(parts, ", ")
}

var names struct {
	sync.Mutex
	m map[int32]nameEntry
}

type nameEntry struct {
	name string
	at   time.Time
}

func processName(ctx context.Context, pid int32) string {
	if pid <= 0 {
		return ""
	}
	names.Lock()
	if e, ok := names.m[pid]; ok && time.Since(e.at) < time.Minute {
		names.Unlock()
		return e.name
	}
	names.Unlock()
	var n string
	if p, err := process.NewProcessWithContext(ctx, pid); err == nil {
		n, _ = p.NameWithContext(ctx)
	}
	names.Lock()
	if names.m == nil {
		names.m = map[int32]nameEntry{}
	}
	names.m[pid] = nameEntry{n, time.Now()}
	names.Unlock()
	return n
}

// Free reports whether proto/port can be bound on ip right now.
func Free(proto, ip string, port uint16) bool {
	addr := net.JoinHostPort(ip, strconv.Itoa(int(port)))
	if proto == "tcp" {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		l.Close()
		return true
	}
	c, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
