// Package ui serves TailLAN's interface: a small web app on
// 127.0.0.1, shown in an app-style browser window. Using the system
// browser keeps TailLAN a single static binary with no GUI toolkit to
// install, on every OS.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LaneBirmingham/taillan/internal/config"
	"github.com/LaneBirmingham/taillan/internal/doctor"
	"github.com/LaneBirmingham/taillan/internal/helpers"
	"github.com/LaneBirmingham/taillan/internal/invite"
	"github.com/LaneBirmingham/taillan/internal/launch"
	"github.com/LaneBirmingham/taillan/internal/portscan"
	"github.com/LaneBirmingham/taillan/internal/profile"
	"github.com/LaneBirmingham/taillan/internal/session"
)

//go:embed web
var webFS embed.FS

// DownloadURL, if set (at build time via -ldflags "-X
// taillan/internal/ui.DownloadURL=..."), is included in the message
// hosts copy for friends, so friends know where to get TailLAN.
var DownloadURL = ""

// App is the running UI backend and owner of the current session.
type App struct {
	Store *profile.Store
	Log   *session.LogBuffer
	Token string
	URL   string // base URL, no token
	Quit  func()
	// Headless serves only the API, not the app's pages: for
	// "taillan host" and "taillan join" in a terminal, which other
	// taillan commands (status, lock, kick, stop...) talk to.
	Headless bool
	settings *config.Settings

	mu      sync.Mutex
	host    *session.Host
	join    *session.Join
	ended   *session.JoinStatus // last join session, after it ended
	viewers int
	lastUI  time.Time
	hostCfg *session.HostConfig
	// starting is set while a host or join session is being set up
	// (that takes seconds), so a second request can't start another
	// one alongside it. stopping asks the one being set up to stop as
	// soon as it's ready.
	starting, stopping bool
}

// claimStart reserves the single session slot, or says why it's taken.
func (a *App) claimStart() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.starting {
		return errors.New("already starting a game; wait a moment")
	}
	if a.host != nil || (a.join != nil && !a.join.Ended()) {
		return errors.New("stop the current session first")
	}
	a.starting, a.stopping, a.ended, a.join = true, false, nil, nil
	return nil
}

// finishStart releases the slot claimed by claimStart. It reports
// whether the session should keep running (nobody stopped it while it
// was starting) and, if so, records it with set while still locked.
func (a *App) finishStart(set func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := !a.stopping
	a.starting, a.stopping = false, false
	if keep && set != nil {
		set()
	}
	return keep
}

// NewApp prepares the backend.
func NewApp(store *profile.Store, log *session.LogBuffer, settings *config.Settings) *App {
	b := make([]byte, 16)
	rand.Read(b)
	return &App{Store: store, Log: log, settings: settings, Token: hex.EncodeToString(b), lastUI: time.Now()}
}

// LaunchURL is the URL that opens the UI (it carries the token once,
// which is then kept in a cookie).
func (a *App) LaunchURL() string { return a.URL + "/?t=" + a.Token }

// Listen binds the UI server on 127.0.0.1. It tries a stable port
// first so browser windows from an earlier run keep working.
func (a *App) Listen() (net.Listener, error) {
	ln, err := a.ListenAddr("127.0.0.1:47800")
	if err != nil {
		ln, err = a.ListenAddr("127.0.0.1:0")
	}
	return ln, err
}

// ListenAddr binds the server on addr, which must be a loopback
// address: the API controls sessions, so it is never on the network.
func (a *App) ListenAddr(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || ip.To4() == nil {
		return nil, fmt.Errorf("the TailLAN API only listens on 127.0.0.1, not %q", addr)
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, err
	}
	a.URL = "http://" + ln.Addr().String()
	return ln, nil
}

// Serve runs the HTTP server until ctx is done.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Idle reports whether nobody has had the UI open for d and nothing
// is being shared or joined, so the process can exit.
func (a *App) Idle(d time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	busy := a.starting || a.host != nil || (a.join != nil && !a.join.Ended())
	return !busy && a.viewers == 0 && time.Since(a.lastUI) > d
}

// Shutdown ends any session.
func (a *App) Shutdown() {
	a.mu.Lock()
	h, j := a.host, a.join
	a.host, a.join = nil, nil
	a.stopping = a.starting
	a.mu.Unlock()
	if h != nil {
		h.Stop()
	}
	if j != nil {
		j.Stop()
	}
}

// Handler returns the HTTP handler.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if a.Headless {
			http.Error(w, "This TailLAN runs in a terminal. Manage it with \"taillan status\", or run \"taillan\" for the app.", http.StatusNotFound)
			return
		}
		if t := r.URL.Query().Get("t"); t != "" {
			if !a.tokenOK(t) {
				http.Error(w, "This TailLAN link is out of date. Open TailLAN again.", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "taillan", Value: a.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if r.URL.Path == "/" && !a.authed(r) {
			http.Error(w, "Open TailLAN from its app icon or terminal to get a fresh link.", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		if !a.tokenOK(r.URL.Query().Get("t")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		io.WriteString(w, "ok")
	})
	api := func(pattern string, fn func(r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !a.authed(r) {
				http.Error(w, `{"error":"not authorized; reopen TailLAN"}`, http.StatusForbidden)
				return
			}
			v, err := fn(r)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			if v == nil {
				v = map[string]bool{"ok": true}
			}
			json.NewEncoder(w).Encode(v)
		})
	}
	api("GET /api/state", func(*http.Request) (any, error) { return a.state(), nil })
	api("GET /api/games", func(*http.Request) (any, error) { return a.games(), nil })
	api("POST /api/games", a.saveGame)
	api("POST /api/games/delete", a.deleteGame)
	api("POST /api/ports/parse", a.parsePorts)
	api("POST /api/host", a.startHost)
	api("POST /api/host/stop", a.stopHost)
	api("POST /api/host/lock", a.lockHost)
	api("POST /api/host/kick", a.kick)
	api("POST /api/host/target", a.setTarget)
	api("POST /api/host/forget", a.forgetInvite)
	api("POST /api/invite/inspect", a.inspect)
	api("POST /api/join", a.startJoin)
	api("POST /api/join/stop", a.stopJoin)
	api("GET /api/detect", a.detect)
	api("GET /api/doctor", func(r *http.Request) (any, error) { return doctor.Run(r.Context()), nil })
	api("POST /api/settings", a.saveSettings)
	api("GET /api/logs", func(*http.Request) (any, error) { return a.Log.Lines(), nil })
	api("GET /api/helpers", func(*http.Request) (any, error) { return helpers.List(), nil })
	api("POST /api/open", a.openFolder)
	api("POST /api/quit", func(*http.Request) (any, error) {
		go func() { time.Sleep(200 * time.Millisecond); a.Quit() }()
		return nil, nil
	})
	mux.HandleFunc("GET /api/events", a.events)
	return a.guard(mux)
}

// guard rejects requests that didn't come from our own page: wrong
// Host (DNS rebinding) or a foreign Origin (CSRF).
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.TrimPrefix(a.URL, "http://")
		_, port, _ := net.SplitHostPort(host)
		okHost := r.Host == host || r.Host == "localhost:"+port
		if !okHost {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "bad origin", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *App) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(a.Token)) == 1
}

func (a *App) authed(r *http.Request) bool {
	c, err := r.Cookie("taillan")
	return err == nil && a.tokenOK(c.Value)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad request: %v", err)
	}
	return nil
}

// State is everything the UI renders.
type State struct {
	Version  string   `json:"version"`
	Download string   `json:"downloadURL,omitempty"`
	Name     string   `json:"name"`
	Verbose  bool     `json:"verbose"`
	Session  any      `json:"session"`
	GamesDir string   `json:"gamesDir"`
	Problems []string `json:"problems,omitempty"`
}

func (a *App) state() *State {
	a.mu.Lock()
	h, j, ended := a.host, a.join, a.ended
	s := &State{Version: session.Version, Download: DownloadURL, Name: a.settings.Name, Verbose: a.settings.Verbose, GamesDir: a.Store.Dir()}
	a.mu.Unlock()
	switch {
	case h != nil:
		s.Session = h.Status()
	case j != nil:
		st := j.Status()
		if j.Ended() {
			a.mu.Lock()
			if a.join == j {
				a.join, a.ended = nil, st
			}
			a.mu.Unlock()
		}
		s.Session = st
	case ended != nil:
		s.Session = ended
	}
	s.Problems = a.Store.Problems()
	return s
}

func (a *App) events(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	a.mu.Lock()
	a.viewers++
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.viewers--
		a.lastUI = time.Now()
		a.mu.Unlock()
	}()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		b, _ := json.Marshal(a.state())
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); err != nil {
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

// GameInfo is a profile plus display extras.
type GameInfo struct {
	*profile.Profile
	PortSummary string `json:"portSummary"`
	HasSaved    bool   `json:"hasSavedInvite"`
}

func (a *App) games() []GameInfo {
	var out []GameInfo
	for _, p := range a.Store.All() {
		hk, _ := config.LoadHostKey("game-" + p.ID)
		out = append(out, GameInfo{Profile: p, PortSummary: p.PortSummary(), HasSaved: hk != nil})
	}
	return out
}

func (a *App) saveGame(r *http.Request) (any, error) {
	var req struct {
		Profile   *profile.Profile `json:"profile"`
		PortsSpec string           `json:"portsSpec"`
		Steps     string           `json:"joinSteps"`
		HostSteps string           `json:"hostSteps"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	p := req.Profile
	if p == nil {
		return nil, errors.New("missing game")
	}
	if req.PortsSpec != "" {
		ports, err := profile.ParsePorts(req.PortsSpec)
		if err != nil {
			return nil, err
		}
		p.Ports = ports
	}
	if req.Steps != "" {
		p.Join.Steps = lines(req.Steps)
	}
	if req.HostSteps != "" {
		p.Host.Steps = lines(req.HostSteps)
	}
	if p.ID == "" {
		p.ID = profile.Slug(p.Name)
	}
	path, err := a.Store.Save(p)
	if err != nil {
		return nil, err
	}
	a.Log.Logf("Saved game %q to %s", p.Name, path)
	return map[string]string{"id": p.ID, "path": path}, nil
}

func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (a *App) deleteGame(r *http.Request) (any, error) {
	var req struct{ ID string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return nil, a.Store.Delete(req.ID)
}

func (a *App) parsePorts(r *http.Request) (any, error) {
	var req struct{ Spec string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ports, err := profile.ParsePorts(req.Spec)
	if err != nil {
		return nil, err
	}
	p := &profile.Profile{ID: "x", Name: "x", Ports: ports}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return map[string]any{"ports": ports, "summary": p.PortSummary()}, nil
}

// HostRequest starts sharing.
type HostRequest struct {
	GameID     string         `json:"gameId"`
	CustomName string         `json:"customName"`
	CustomSpec string         `json:"customPorts"`
	ExtraPorts string         `json:"extraPorts"`
	TargetIP   string         `json:"targetIP"`
	Targets    map[int]uint16 `json:"targets"`
	KeepCode   bool           `json:"keepCode"`
}

func (a *App) startHost(r *http.Request) (any, error) {
	var req HostRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	p, err := a.ResolveGame(req.GameID, req.CustomName, req.CustomSpec, req.ExtraPorts)
	if err != nil {
		return nil, err
	}
	h, err := a.StartHosting(r.Context(), session.HostConfig{
		Profile:  p,
		TargetIP: strings.TrimSpace(req.TargetIP),
		Targets:  req.Targets,
		KeepCode: req.KeepCode,
		HostName: a.settings.Name,
		Verbose:  a.settings.Verbose,
	})
	if err != nil {
		return nil, err
	}
	return h.Status(), nil
}

// StartHosting starts sharing as the app's one session, logging to the
// app's log. It fails if a session is already running or starting.
func (a *App) StartHosting(ctx context.Context, cfg session.HostConfig) (*session.Host, error) {
	if err := a.claimStart(); err != nil {
		return nil, err
	}
	cfg.Log = a.Log
	h, err := session.StartHost(ctx, cfg)
	if err != nil {
		a.finishStart(nil)
		a.Log.Logf("Couldn't start sharing: %v", err)
		return nil, err
	}
	if !a.finishStart(func() { a.host, a.hostCfg = h, &cfg }) {
		h.Stop()
		return nil, errors.New("sharing was stopped before it started")
	}
	return h, nil
}

// ResolveGame builds the profile to host from a saved game id or a
// custom name and port spec, plus optional extra ports.
func (a *App) ResolveGame(id, customName, customSpec, extra string) (*profile.Profile, error) {
	var p *profile.Profile
	if id != "" && id != "custom" {
		g := a.Store.Get(id)
		if g == nil {
			return nil, fmt.Errorf("unknown game %q", id)
		}
		c := *g
		c.Ports = append([]profile.Port(nil), g.Ports...)
		p = &c
	} else {
		name := strings.TrimSpace(customName)
		if name == "" {
			name = "Custom game"
		}
		ports, err := profile.ParsePorts(customSpec)
		if err != nil {
			return nil, fmt.Errorf("ports: %v", err)
		}
		p = &profile.Profile{ID: profile.Slug(name), Name: name, Ports: ports, Source: "custom"}
	}
	if strings.TrimSpace(extra) != "" {
		more, err := profile.ParsePorts(extra)
		if err != nil {
			return nil, fmt.Errorf("extra ports: %v", err)
		}
		for i := range more {
			more[i].Label = "Extra"
		}
		p.Ports = append(p.Ports, more...)
		if p.Source == "builtin" {
			// The friend's built-in copy wouldn't know the extra ports'
			// purpose; the invite carries ports regardless.
			p.Source = "builtin+extra"
		}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func (a *App) currentHost() (*session.Host, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.host == nil {
		return nil, errors.New("not hosting")
	}
	return a.host, nil
}

func (a *App) stopHost(*http.Request) (any, error) {
	a.mu.Lock()
	h := a.host
	a.host = nil
	a.stopping = a.starting
	a.mu.Unlock()
	if h != nil {
		h.Stop()
	}
	return nil, nil
}

func (a *App) lockHost(r *http.Request) (any, error) {
	var req struct{ Locked bool }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	h, err := a.currentHost()
	if err != nil {
		return nil, err
	}
	h.SetLocked(req.Locked)
	return nil, nil
}

func (a *App) kick(r *http.Request) (any, error) {
	var req struct{ ID string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	h, err := a.currentHost()
	if err != nil {
		return nil, err
	}
	return nil, h.Kick(req.ID)
}

func (a *App) setTarget(r *http.Request) (any, error) {
	var req struct {
		Group int    `json:"group"`
		Port  uint16 `json:"port"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	h, err := a.currentHost()
	if err != nil {
		return nil, err
	}
	if req.Port == 0 {
		return nil, errors.New("port must be 1-65535")
	}
	h.SetTarget(req.Group, req.Port, "")
	return nil, nil
}

func (a *App) forgetInvite(r *http.Request) (any, error) {
	var req struct{ GameID string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	a.Log.Logf("Retired the saved invite for %s", req.GameID)
	return nil, config.DeleteHostKey("game-" + profile.Slug(req.GameID))
}

// InviteInfo previews an invite before joining.
type InviteInfo struct {
	HostName    string           `json:"hostName"`
	Game        *profile.Profile `json:"game"`
	PortSummary string           `json:"portSummary"`
	Known       bool             `json:"known"` // this TailLAN has the game built in
	Busy        []string         `json:"busy,omitempty"`
}

func (a *App) inspect(r *http.Request) (any, error) {
	var req struct{ Code string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	inv, err := invite.Decode(req.Code)
	if err != nil {
		return nil, err
	}
	local := a.Store.Builtin(inv.GameID)
	p, err := session.JoinProfile(inv, local)
	if err != nil {
		return nil, err
	}
	info := &InviteInfo{HostName: inv.HostName, Game: p, PortSummary: p.PortSummary(), Known: local != nil}
	for _, pt := range p.Ports {
		if !pt.SamePort {
			continue
		}
		for _, proto := range []string{"tcp", "udp"} {
			if (pt.Proto == proto || pt.Proto == "both") && !portscan.Free(proto, "127.0.0.1", pt.Port) {
				info.Busy = append(info.Busy, strconv.Itoa(int(pt.Port))+"/"+proto)
			}
		}
	}
	return info, nil
}

func (a *App) startJoin(r *http.Request) (any, error) {
	var req struct {
		Code     string `json:"code"`
		AllowLAN bool   `json:"allowLAN"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	inv, err := invite.Decode(req.Code)
	if err != nil {
		return nil, err
	}
	p, err := session.JoinProfile(inv, a.Store.Builtin(inv.GameID))
	if err != nil {
		return nil, err
	}
	j, err := a.StartJoining(session.JoinConfig{
		Invite:   inv,
		Profile:  p,
		Name:     a.settings.Name,
		Key:      a.settings.ClientPrivateKey(),
		AllowLAN: req.AllowLAN,
		Verbose:  a.settings.Verbose,
	})
	if err != nil {
		return nil, err
	}
	return j.Status(), nil
}

// StartJoining joins a game as the app's one session, logging to the
// app's log. It fails if a session is already running or starting.
func (a *App) StartJoining(cfg session.JoinConfig) (*session.Join, error) {
	if err := a.claimStart(); err != nil {
		return nil, err
	}
	cfg.Log = a.Log
	j, err := session.StartJoin(cfg)
	if err != nil {
		a.finishStart(nil)
		a.Log.Logf("Couldn't join: %v", err)
		return nil, err
	}
	if !a.finishStart(func() { a.join = j }) {
		j.Stop()
		return nil, errors.New("left the game before it started")
	}
	return j, nil
}

func (a *App) stopJoin(*http.Request) (any, error) {
	a.mu.Lock()
	j := a.join
	a.join, a.ended = nil, nil
	a.stopping = a.starting
	a.mu.Unlock()
	if j != nil {
		j.Stop()
	}
	return nil, nil
}

func (a *App) detect(r *http.Request) (any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	socks, err := portscan.Listening(ctx)
	if err != nil {
		return nil, fmt.Errorf("couldn't list ports: %v", err)
	}
	return portscan.ByProcess(socks), nil
}

func (a *App) saveSettings(r *http.Request) (any, error) {
	var req struct {
		Name    *string `json:"name"`
		Verbose *bool   `json:"verbose"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.Name != nil {
		n := config.CleanName(*req.Name)
		if n == "" {
			return nil, errors.New("name can't be empty")
		}
		a.settings.Name = n
	}
	if req.Verbose != nil {
		a.settings.Verbose = *req.Verbose
	}
	return nil, config.Save(a.settings)
}

func (a *App) openFolder(r *http.Request) (any, error) {
	var req struct{ Which string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	dir := config.Dir()
	if req.Which == "games" {
		dir = a.Store.Dir()
	}
	return map[string]string{"path": dir}, launch.OpenFolder(dir)
}
