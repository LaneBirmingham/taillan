package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LaneBirmingham/taillan/internal/config"
	"github.com/LaneBirmingham/taillan/internal/profile"
	"github.com/LaneBirmingham/taillan/internal/session"
	"github.com/LaneBirmingham/taillan/internal/ui"
)

// newApp runs a real headless app API, as "taillan host" does.
func newApp(t *testing.T) (*ui.App, *httptest.Server) {
	t.Helper()
	store, err := profile.NewStore(config.GamesDir())
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := config.Load()
	a := ui.NewApp(store, session.NewLogBuffer(nil), settings)
	a.Headless = true
	srv := httptest.NewServer(nil)
	a.URL = srv.URL
	srv.Config.Handler = a.Handler()
	t.Cleanup(srv.Close)
	return a, srv
}

func TestRegisterAndRunning(t *testing.T) {
	t.Setenv("TAILLAN_HOME", t.TempDir())
	ctx := context.Background()
	a, _ := newApp(t)
	live, unregister, err := Register(Instance{URL: a.URL, Token: a.Token, PID: 1001, Kind: "host", Game: "valheim", Keep: true, Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(live.file)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("record readable by others: %v, %v", fi.Mode(), err)
		}
		di, _ := os.Stat(Dir())
		if di.Mode().Perm() != 0o700 {
			t.Errorf("run dir mode %v", di.Mode())
		}
	}

	// A crashed instance: nothing listens on its port any more.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	Register(Instance{URL: deadURL, Token: "x", PID: 1002, Kind: "host"})
	// The port now belongs to an instance with a different secret.
	Register(Instance{URL: a.URL, Token: "stale", PID: 1003, Kind: "app"})
	os.WriteFile(filepath.Join(Dir(), "1004.json"), []byte("not json"), 0o600)
	// Another container sharing this folder, in its own network
	// namespace: its 127.0.0.1 isn't ours, so its record stays.
	other, _ := json.Marshal(Instance{URL: deadURL, Token: "y", PID: 1, Kind: "host", NetNS: "net:[1]"})
	os.WriteFile(filepath.Join(Dir(), "1-other.json"), other, 0o600)

	got := Running(ctx)
	if len(got) != 1 || got[0].PID != 1001 {
		t.Fatalf("Running = %+v, want only PID 1001", got)
	}
	if !got[0].Is(live) || got[0].Game != "valheim" || !got[0].Keep {
		t.Errorf("record = %+v, want %+v", got[0], live)
	}
	left, _ := filepath.Glob(filepath.Join(Dir(), "*.json"))
	if len(left) != 2 {
		t.Errorf("stale records not cleaned up, or another namespace's removed: %v", left)
	}

	st, err := got[0].State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active() || st.Host != nil || st.Join != nil {
		t.Errorf("idle app reports a session: %+v", st)
	}
	// Actions reach the app and its errors come back as they are.
	if err := got[0].Post(ctx, "/api/host/lock", map[string]bool{"locked": true}); err == nil || err.Error() != "not hosting" {
		t.Errorf("lock while not hosting: %v", err)
	}
	if err := got[0].Post(ctx, "/api/host/stop", nil); err != nil {
		t.Errorf("stop: %v", err)
	}

	unregister()
	if len(Running(ctx)) != 0 {
		t.Error("unregistered instance still listed")
	}
}

// TestRegisterSamePID checks two records with one PID (each container's
// TailLAN is PID 1) don't overwrite each other, and each removes only
// its own.
func TestRegisterSamePID(t *testing.T) {
	t.Setenv("TAILLAN_HOME", t.TempDir())
	a, _ := newApp(t)
	b, _ := newApp(t)
	ra, unregA, err := Register(Instance{URL: a.URL, Token: a.Token, PID: 1, Kind: "host"})
	if err != nil {
		t.Fatal(err)
	}
	rb, unregB, err := Register(Instance{URL: b.URL, Token: b.Token, PID: 1, Kind: "join"})
	if err != nil {
		t.Fatal(err)
	}
	defer unregB()
	if ra.Is(rb) {
		t.Fatal("same record file for both")
	}
	if n := len(Running(context.Background())); n != 2 {
		t.Fatalf("Running = %d instances, want 2", n)
	}
	unregA()
	got := Running(context.Background())
	if len(got) != 1 || !got[0].Is(rb) {
		t.Errorf("after one unregistered: %+v", got)
	}
}

// TestState checks a session's status is decoded by its role.
func TestState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {})
	body := map[string]string{
		"host": `{"version":"v1","name":"Lane","session":{"role":"host","game":{"id":"valheim","name":"Valheim"},"invite":"TL1-x","locked":true,"friends":[{"id":"nodekey:ab","name":"Sam","online":true}]}}`,
		"join": `{"version":"v1","session":{"role":"join","phase":"ended","game":{"id":"factorio","name":"Factorio"},"connect":"127.0.0.1:34197"}}`,
	}
	var serve string
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("taillan"); err != nil || c.Value != "secret" {
			http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
			return
		}
		w.Write([]byte(body[serve]))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	in := Instance{URL: srv.URL, Token: "secret", PID: 7}

	serve = "host"
	st, err := in.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Host == nil || st.Host.Invite != "TL1-x" || !st.Host.Locked || st.Host.Friends[0].Name != "Sam" || st.GameID() != "valheim" || !st.Active() {
		t.Errorf("host state: %+v", st.Host)
	}

	serve = "join"
	st, err = in.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Join == nil || st.GameID() != "factorio" || st.Active() {
		t.Errorf("ended join: %+v, active %v", st.Join, st.Active())
	}

	in.Token = "wrong"
	if _, err := in.State(context.Background()); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("wrong secret: %v", err)
	}
}
