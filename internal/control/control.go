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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"

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
	// Game and Keep are what a "taillan host" was asked to share, known
	// before it's sharing (it may be waiting for the network).
	Game string `json:"game,omitempty"`
	Keep bool   `json:"keep,omitempty"`
	// NetNS names the network namespace (Linux) the instance runs in.
	// Its 127.0.0.1 is only reachable from the same one, so records
	// from other containers sharing this folder are left alone.
	NetNS string `json:"netns,omitempty"`

	file string
}

// Dir is where running instances are recorded.
func Dir() string { return filepath.Join(config.Dir(), "run") }

// Is reports whether two Instances are the same record.
func (in Instance) Is(other Instance) bool {
	return in.file != "" && in.file == other.file
}

// netNS identifies this process's network namespace, or "" where
// there are none (outside Linux).
func netNS() string {
	ns, _ := os.Readlink("/proc/self/ns/net")
	return ns
}

// A running instance touches its record every heartbeat. A record from
// another network namespace (another container) can't be pinged, so
// one not touched for staleAfter is taken to be from an instance that's
// gone (a container that was killed or recreated) and removed.
var (
	heartbeat  = time.Minute
	staleAfter = 10 * time.Minute
)

// Register records a running instance so other taillan commands can
// find it. It returns the record and a function that removes it; call
// that on exit.
func Register(in Instance) (Instance, func(), error) {
	in.NetNS = netNS()
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return in, func() {}, err
	}
	data, err := json.Marshal(in)
	if err != nil {
		return in, func() {}, err
	}
	// The PID alone isn't unique: in containers, it's 1 every time.
	b := make([]byte, 4)
	rand.Read(b)
	in.file = filepath.Join(Dir(), fmt.Sprintf("%d-%x.json", in.PID, b))
	if err := writeRecord(in.file, data); err != nil {
		return in, func() {}, err
	}
	var mu sync.Mutex
	stopped := false
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			mu.Lock()
			if !stopped {
				now := time.Now()
				if err := os.Chtimes(in.file, now, now); errors.Is(err, fs.ErrNotExist) {
					// Removed while we're still here (taken for stale
					// after a long sleep): put it back.
					writeRecord(in.file, data)
				}
			}
			mu.Unlock()
		}
	}()
	return in, func() {
		mu.Lock()
		defer mu.Unlock()
		if !stopped {
			stopped = true
			close(done)
			os.Remove(in.file)
		}
	}, nil
}

func writeRecord(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Running returns the instances that answer, oldest first. Records of
// instances that are gone (crashed, or killed) are removed.
func Running(ctx context.Context) []Instance {
	files, _ := filepath.Glob(filepath.Join(Dir(), "*.json"))
	here := netNS()
	found := make([]*Instance, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var in Instance
		if json.Unmarshal(data, &in) != nil || in.URL == "" || in.Token == "" {
			os.Remove(f)
			continue
		}
		if in.NetNS != here {
			// Another container's: unreachable from here, so only its
			// heartbeat says whether it's still there.
			if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > staleAfter {
				os.Remove(f)
			}
			continue
		}
		in.file = f
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch in.check(ctx) {
			case alive:
				found[i] = &in
			case gone:
				os.Remove(f)
			}
		}()
	}
	wg.Wait()
	var out []Instance
	for _, in := range found {
		if in != nil {
			out = append(out, *in)
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

// wsaeConnRefused is Windows' "connection refused", which isn't
// syscall.ECONNREFUSED there.
const wsaeConnRefused = 10061

func (in Instance) check(ctx context.Context) liveness {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, "GET", in.URL+"/api/ping?t="+in.Token, nil)
	if err != nil {
		return gone
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return alive
		}
		return gone // something else took the port, or a different secret
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == syscall.ECONNREFUSED || errno == wsaeConnRefused) {
		return gone
	}
	// No clear answer (a timeout, a firewall): only a process that's
	// gone settles it. PIDs are only checked here, after the ping,
	// because a record can come from another PID namespace.
	if in.PID > 0 {
		if ok, err := process.PidExistsWithContext(ctx, int32(in.PID)); err == nil && !ok {
			return gone
		}
	}
	return unsure
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
