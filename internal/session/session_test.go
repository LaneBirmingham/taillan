package session

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"tailscale.com/types/key"

	"github.com/LaneBirmingham/taillan/internal/invite"
	"github.com/LaneBirmingham/taillan/internal/profile"
)

func TestRender(t *testing.T) {
	p := &profile.Profile{Ports: []profile.Port{{Port: 7777, Proto: "tcp"}, {Port: 8888, Proto: "udp"}}}
	vars := joinVars(p, "Sam", []uint16{7778, 8888})
	if got := vars["connect"]; got != "127.0.0.1:7778" {
		t.Errorf("connect = %q", got)
	}
	got := Render("Ask {{hostname}}; type {{connect}} then port {{port2}}", vars)
	if want := "Ask Sam; type 127.0.0.1:7778 then port 8888"; got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

// TestHostJoinEndToEnd runs a real host and joiner in one process,
// connected through tailcat's public relays, and checks that TCP and
// UDP game traffic both make it through. It needs internet access;
// set TAILLAN_SKIP_NET=1 to skip it.
func TestHostJoinEndToEnd(t *testing.T) {
	if testing.Short() || os.Getenv("TAILLAN_SKIP_NET") != "" {
		t.Skip("needs internet access")
	}
	t.Setenv("TAILLAN_HOME", t.TempDir())

	// A fake "game server": TCP and UDP echo on the same port.
	tl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	port := uint16(tl.Addr().(*net.TCPAddr).Port)
	ul, err := net.ListenPacket("udp4", "127.0.0.1:"+strconv.Itoa(int(port)))
	if err != nil {
		t.Skipf("UDP port %d busy: %v", port, err)
	}
	defer ul.Close()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, a, err := ul.ReadFrom(buf)
			if err != nil {
				return
			}
			ul.WriteTo(append([]byte("echo:"), buf[:n]...), a)
		}
	}()

	game := &profile.Profile{
		ID:     "test-game",
		Name:   "Test Game",
		Ports:  []profile.Port{{Port: port, Proto: "both", Label: "Game"}},
		Source: "user",
		Join:   profile.Guide{Steps: []string{"Connect to {{connect}}"}},
	}
	log := NewLogBuffer(func(s string) { t.Log(s) })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := StartHost(ctx, HostConfig{Profile: game, HostName: "Hosty", Log: log})
	if err != nil {
		t.Fatalf("StartHost: %v", err)
	}
	defer h.Stop()

	inv, err := invite.Decode("join me! " + h.Invite() + " :)")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	jp, err := JoinProfile(inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, err := StartJoin(JoinConfig{Invite: inv, Profile: jp, Name: "Joiny", Key: key.NewNode(), Log: log})
	if err != nil {
		t.Fatalf("StartJoin: %v", err)
	}
	defer j.Stop()

	// The fake server holds the port here, so the joiner must have
	// moved to another one.
	st := j.Status()
	if j.groupLocal[0] == port {
		t.Fatalf("joiner reused busy port %d", port)
	}
	if !strings.Contains(st.Steps[0], strconv.Itoa(int(j.groupLocal[0]))) {
		t.Errorf("steps don't mention the remapped port: %q", st.Steps)
	}
	waitFor(t, ctx, "joiner connected", func() bool { return j.Status().Phase == "connected" })

	local := "127.0.0.1:" + strconv.Itoa(int(j.groupLocal[0]))
	c, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	msg := []byte("hello over tcp")
	c.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("TCP echo: got %q, err %v", got, err)
	}
	c.Close()

	uc, err := net.Dial("udp4", local)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	buf := make([]byte, 2048)
	ok := false
	for i := 0; i < 20 && !ok; i++ {
		uc.Write([]byte("ping"))
		uc.SetReadDeadline(time.Now().Add(time.Second))
		n, err := uc.Read(buf)
		ok = err == nil && string(buf[:n]) == "echo:ping"
	}
	if !ok {
		t.Fatal("no UDP echo through the tunnel")
	}

	waitFor(t, ctx, "host sees friend by name", func() bool {
		for _, f := range h.Status().Friends {
			if f.Name == "Joiny" && f.Online && f.App == "TailLAN" {
				return true
			}
		}
		return false
	})
	if g := h.Status().Groups[0]; g.Listening != "yes" {
		t.Errorf("host says game listening = %q, want yes", g.Listening)
	}

	// Host stopping ends the joiner's session with a reason.
	h.Stop()
	waitFor(t, ctx, "joiner sees session end", func() bool { return j.Ended() })
	if m := j.Status().Message; !strings.Contains(m, "stopped") {
		t.Errorf("end message = %q", m)
	}
}

func waitFor(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func TestJoinProfile(t *testing.T) {
	local := &profile.Profile{
		ID: "g", Name: "G", Ports: []profile.Port{{Port: 1000, Proto: "udp"}},
		Join:    profile.Guide{Steps: []string{"local step"}},
		Notes:   "local note",
		Helpers: []string{"minecraft-lan"},
		Links:   []profile.Link{{Title: "Docs", URL: "https://example.com"}},
	}
	base := invite.Invite{Version: 1, Addr: "tc-test", GameID: "g", GameName: "G",
		Ports: []invite.Port{{Port: 1000, Proto: "u"}}}

	// Built-in games travel without steps: the local copy fills them in.
	inv := base
	p, err := JoinProfile(&inv, local)
	if err != nil {
		t.Fatal(err)
	}
	if p.Join.Steps[0] != "local step" || p.Notes != "local note" || len(p.Helpers) != 1 || len(p.Links) != 1 {
		t.Errorf("local details not filled in: %+v", p)
	}

	// The host's own version of the game wins over the local one.
	inv = base
	inv.Steps, inv.Notes = []string{"host step"}, "host note"
	p, _ = JoinProfile(&inv, local)
	if p.Join.Steps[0] != "host step" || p.Notes != "host note" {
		t.Errorf("invite details overridden: %+v", p)
	}

	// No local copy: a default step that mentions the address.
	inv = base
	p, _ = JoinProfile(&inv, nil)
	if len(p.Join.Steps) != 1 || !strings.Contains(p.Join.Steps[0], "{{connect}}") {
		t.Errorf("default steps: %v", p.Join.Steps)
	}
}
