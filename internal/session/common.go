// Package session runs TailLAN's two roles over tailcat:
//
//   - A Host runs a tailcat server that forwards a game's ports to the
//     game server on the host's machine, and hands out an invite code.
//   - A Join decodes an invite, connects to the host, and opens the
//     same ports on the friend's own machine (127.0.0.1), so the game
//     connects to "localhost" as if the server were local.
//
// Both sides speak a tiny JSON-lines control protocol on
// profile.ControlPort inside the tunnel, for names, latency, and a
// clean "host ended the session" signal.
package session

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LaneBirmingham/taillan/internal/profile"
)

// Version is TailLAN's version, set by main.
var Version = "dev"

// LogBuffer keeps recent log lines for the UI's Logs panel.
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	echo  func(string) // optional mirror (the terminal)
}

// NewLogBuffer returns a buffer that also passes each line to echo, if
// non-nil.
func NewLogBuffer(echo func(string)) *LogBuffer { return &LogBuffer{echo: echo} }

// Logf records a line.
func (l *LogBuffer) Logf(format string, args ...any) {
	if l == nil {
		return
	}
	line := time.Now().Format("15:04:05 ") + strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > 600 {
		l.lines = append([]string(nil), l.lines[len(l.lines)-500:]...)
	}
	echo := l.echo
	l.mu.Unlock()
	if echo != nil {
		echo(line)
	}
}

// Lines returns a copy of the buffered lines.
func (l *LogBuffer) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.lines...)
}

// ctlMsg is one control-protocol message, sent as a JSON line.
type ctlMsg struct {
	T       string  `json:"t"` // hello, welcome, ping, pong, bye
	Name    string  `json:"name,omitempty"`
	Version string  `json:"v,omitempty"`
	N       int64   `json:"n,omitempty"`
	RTT     float64 `json:"rtt,omitempty"`  // client-measured round trip, ms
	Path    string  `json:"path,omitempty"` // client's view of the path
	Reason  string  `json:"reason,omitempty"`
	Game    string  `json:"game,omitempty"`
}

func writeMsg(w interface{ Write([]byte) (int, error) }, m ctlMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// Render fills an instruction template. vars maps placeholder names
// (without braces) to values.
func Render(tmpl string, vars map[string]string) string {
	if !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

// joinVars builds the placeholders for join instructions, given the
// local base port of each port group.
func joinVars(p *profile.Profile, hostName string, groupPorts []uint16) map[string]string {
	vars := map[string]string{"host": "127.0.0.1", "hostname": hostName}
	if hostName == "" {
		vars["hostname"] = "your friend"
	}
	for i, port := range groupPorts {
		s := strconv.Itoa(int(port))
		if i == 0 {
			vars["port"] = s
		}
		vars["port"+strconv.Itoa(i+1)] = s
	}
	vars["connect"] = Render(p.ConnectTemplate(), vars)
	return vars
}

// PathInfo describes how traffic reaches the other side.
type PathInfo struct {
	Kind      string  `json:"kind"`                // "direct", "relay" or "" (unknown yet)
	Via       string  `json:"via,omitempty"`       // endpoint or relay region
	LatencyMs float64 `json:"latencyMs,omitempty"` // round trip
}

func fmtRange(first, last uint16) string {
	if last > first {
		return fmt.Sprintf("%d-%d", first, last)
	}
	return strconv.Itoa(int(first))
}

func groupLast(pt profile.Port) uint16 {
	if pt.End > pt.Port {
		return pt.End
	}
	return pt.Port
}

func protosOf(pt profile.Port) []string {
	if pt.Proto == "both" {
		return []string{"tcp", "udp"}
	}
	return []string{pt.Proto}
}
