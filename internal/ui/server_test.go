package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LaneBirmingham/taillan/internal/config"
	"github.com/LaneBirmingham/taillan/internal/invite"
	"github.com/LaneBirmingham/taillan/internal/profile"
	"github.com/LaneBirmingham/taillan/internal/session"
)

func newTestApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	t.Setenv("TAILLAN_HOME", t.TempDir())
	store, err := profile.NewStore(config.GamesDir())
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := config.Load()
	a := NewApp(store, session.NewLogBuffer(nil), settings)
	srv := httptest.NewServer(nil)
	a.URL = srv.URL
	srv.Config.Handler = a.Handler()
	t.Cleanup(srv.Close)
	return a, srv
}

func TestAuthAndGuards(t *testing.T) {
	a, srv := newTestApp(t)
	cookie := &http.Cookie{Name: "taillan", Value: a.Token}
	do := func(method, path, origin, host string, withCookie bool) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
		if withCookie {
			req.AddCookie(cookie)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	self := strings.TrimPrefix(srv.URL, "http://")
	cases := []struct {
		name                 string
		method, path, origin string
		host                 string
		cookie               bool
		want                 int
	}{
		{"state with cookie", "GET", "/api/state", "", "", true, 200},
		{"state without cookie", "GET", "/api/state", "", "", false, 403},
		{"page without cookie", "GET", "/", "", "", false, 403},
		{"bad launch token", "GET", "/?t=wrong", "", "", false, 403},
		{"good launch token", "GET", "/?t=" + a.Token, "", "", false, 302},
		{"cross-site POST", "POST", "/api/host/stop", "http://evil.example", "", true, 403},
		{"same-origin POST", "POST", "/api/host/stop", "http://" + self, "", true, 200},
		{"DNS rebinding", "GET", "/api/state", "", "evil.example:80", true, 403},
		{"ping needs token", "GET", "/api/ping", "", "", false, 403},
		{"ping with token", "GET", "/api/ping?t=" + a.Token, "", "", false, 200},
		{"static asset", "GET", "/app.js", "", "", true, 200},
	}
	for _, c := range cases {
		if got := do(c.method, c.path, c.origin, c.host, c.cookie); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestResolveGame(t *testing.T) {
	a, _ := newTestApp(t)
	p, err := a.ResolveGame("valheim", "", "", "64738")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Ports) != 2 || p.Ports[1].Port != 64738 || p.Ports[1].Label != "Extra" {
		t.Errorf("extra ports not added: %+v", p.Ports)
	}
	if a.Store.Get("valheim").Ports[0].Port != 2456 || len(a.Store.Get("valheim").Ports) != 1 {
		t.Error("ResolveGame modified the stored profile")
	}
	p, err = a.ResolveGame("custom", "Space Haven", "7777/udp", "")
	if err != nil || p.ID != "space-haven" || p.Ports[0].Proto != "udp" {
		t.Errorf("custom: %+v, %v", p, err)
	}
	if _, err := a.ResolveGame("custom", "x", "nope", ""); err == nil {
		t.Error("bad ports accepted")
	}
	if _, err := a.ResolveGame("not-a-game", "", "", ""); err == nil {
		t.Error("unknown game accepted")
	}
}

// TestOneSessionAtATime checks that while a session is starting (which
// takes seconds), no second one can start alongside it, and that
// stopping during startup stops the session once it's up instead of
// leaving it running unseen.
func TestOneSessionAtATime(t *testing.T) {
	a, srv := newTestApp(t)
	code, err := invite.New("tc-test", "Sam", a.Store.Get("valheim")).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.claimStart(); err != nil {
		t.Fatal(err)
	}
	if a.Idle(-time.Second) {
		t.Error("app idle while a session is starting")
	}
	body, _ := json.Marshal(map[string]string{"code": code})
	req, _ := http.NewRequest("POST", srv.URL+"/api/join", strings.NewReader(string(body)))
	req.AddCookie(&http.Cookie{Name: "taillan", Value: a.Token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(msg), "already starting") {
		t.Errorf("second session while starting: %d %s", resp.StatusCode, msg)
	}
	if err := a.claimStart(); err == nil {
		t.Error("claimed the session slot twice")
	}

	// Stop while starting: the session that finishes starting is
	// not kept.
	a.Shutdown()
	if a.finishStart(func() { t.Error("stopped session was recorded") }) {
		t.Error("finishStart kept a session stopped during startup")
	}
	if err := a.claimStart(); err != nil {
		t.Errorf("slot not released: %v", err)
	}
	if !a.finishStart(nil) {
		t.Error("finishStart dropped a session nobody stopped")
	}
}
