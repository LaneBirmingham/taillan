// Package control lets one taillan command find and drive another
// that is already running: the app, or "taillan host" and "taillan
// join" in a terminal (on a server, over SSH, in a container).
//
// Every running TailLAN serves its API on 127.0.0.1 behind a random
// secret, and records the address and secret in the config folder's
// run/ directory, which only the same user can read. Commands such as
// "taillan status" and "taillan kick" read those records and use the
// same API the app's window uses.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LaneBirmingham/taillan/internal/config"
	"github.com/LaneBirmingham/taillan/internal/session"
)

// Instance is one running TailLAN.
type Instance struct {
	URL     string    `json:"url"`   // http://127.0.0.1:port
	Token   string    `json:"token"` // the API secret
	PID     int       `json:"pid"`
	Kind    string    `json:"kind"` // "app", "host" or "join"
	Started time.Time `json:"started"`
}

// Dir is where running instances are recorded.
func Dir() string { return filepath.Join(config.Dir(), "run") }

func (in Instance) path() string {
	return filepath.Join(Dir(), strconv.Itoa(in.PID)+".json")
}

// Register records a running instance so other taillan commands can
// find it. The returned function removes the record; call it on exit.
func Register(in Instance) (func(), error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return func() {}, err
	}
	data, err := json.Marshal(in)
	if err != nil {
		return func() {}, err
	}
	p := in.path()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return func() {}, err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return func() {}, err
	}
	return func() { os.Remove(p) }, nil
}

// Running returns the instances that answer, oldest first. Records of
// instances that are gone (crashed, or killed) are removed.
func Running(ctx context.Context) []Instance {
	files, _ := filepath.Glob(filepath.Join(Dir(), "*.json"))
	var out []Instance
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var in Instance
		if json.Unmarshal(data, &in) != nil || in.URL == "" || in.Token == "" {
			os.Remove(f)
			continue
		}
		switch in.ping(ctx) {
		case alive:
			out = append(out, in)
		case gone:
			os.Remove(f)
		}
	}
	slices.SortFunc(out, func(a, b Instance) int { return a.Started.Compare(b.Started) })
	return out
}

type liveness int

const (
	alive  liveness = iota
	gone            // nothing there, or another program
	unsure          // slow to answer; leave its record alone
)

func (in Instance) ping(ctx context.Context) liveness {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", in.URL+"/api/ping?t="+in.Token, nil)
	if err != nil {
		return gone
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused") {
			return gone
		}
		return unsure
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return alive
	}
	return gone // something else took the port, or a different secret
}

// State is what a running instance reports. Exactly one of Host and
// Join is set while a game is shared or joined.
type State struct {
	Version  string `json:"version"`
	Download string `json:"downloadURL,omitempty"`
	Name     string `json:"name"`
	// Session is the raw session status, as the app's window gets it.
	Session json.RawMessage     `json:"session"`
	Host    *session.HostStatus `json:"-"`
	Join    *session.JoinStatus `json:"-"`
}

// Active reports whether a game is being shared, or joined and not
// over.
func (s *State) Active() bool {
	return s.Host != nil || (s.Join != nil && s.Join.Phase != "ended")
}

// GameID is the id of the game being shared or joined, if any.
func (s *State) GameID() string {
	switch {
	case s.Host != nil && s.Host.Game != nil:
		return s.Host.Game.ID
	case s.Join != nil && s.Join.Game != nil:
		return s.Join.Game.ID
	}
	return ""
}

// State fetches the instance's current state.
func (in Instance) State(ctx context.Context) (*State, error) {
	var st State
	if err := in.call(ctx, "GET", "/api/state", nil, &st); err != nil {
		return nil, err
	}
	var role struct{ Role string }
	if len(st.Session) > 0 && json.Unmarshal(st.Session, &role) == nil {
		switch role.Role {
		case "host":
			st.Host = new(session.HostStatus)
			if err := json.Unmarshal(st.Session, st.Host); err != nil {
				return nil, err
			}
		case "join":
			st.Join = new(session.JoinStatus)
			if err := json.Unmarshal(st.Session, st.Join); err != nil {
				return nil, err
			}
		}
	}
	return &st, nil
}

// Post calls one of the instance's actions, such as "/api/host/lock".
func (in Instance) Post(ctx context.Context, path string, body any) error {
	if body == nil {
		body = struct{}{}
	}
	return in.call(ctx, "POST", path, body, nil)
}

func (in Instance) call(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = strings.NewReader(string(b))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, in.URL+path, r)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{Name: "taillan", Value: in.Token})
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach TailLAN (PID %d): %v", in.PID, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("TailLAN (PID %d) answered %s", in.PID, resp.Status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
