package main

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/LaneBirmingham/taillan/internal/session"
)

func newTestFlags() (*flag.FlagSet, *string, *bool, *string) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	target := fs.String("target", "127.0.0.1", "")
	keep := fs.Bool("keep", false, "")
	display := fs.String("display-name", "", "")
	return fs, target, keep, display
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args    []string
		pos     []string
		target  string
		keep    bool
		display string
	}{
		{[]string{"valheim", "--keep"}, []string{"valheim"}, "127.0.0.1", true, ""},
		{[]string{"--keep", "valheim"}, []string{"valheim"}, "127.0.0.1", true, ""},
		{[]string{"--target", "mc", "minecraft"}, []string{"minecraft"}, "mc", false, ""},
		{[]string{"7777/udp", "--target=10.0.0.2", "27015/tcp"}, []string{"7777/udp", "27015/tcp"}, "10.0.0.2", false, ""},
		{[]string{"--keep", "--", "--not-a-flag"}, []string{"--not-a-flag"}, "127.0.0.1", true, ""},
	}
	for _, c := range cases {
		fs, target, keep, display := newTestFlags()
		pos, err := parseArgs(fs, c.args, false)
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if strings.Join(pos, "|") != strings.Join(c.pos, "|") || *target != c.target || *keep != c.keep || *display != c.display {
			t.Errorf("%q: pos %q target %q keep %v display %q", c.args, pos, *target, *keep, *display)
		}
	}
}

func TestFlagsFromEnv(t *testing.T) {
	t.Setenv("TAILLAN_TARGET", "valheim-server")
	t.Setenv("TAILLAN_KEEP", "yes")
	t.Setenv("TAILLAN_DISPLAY_NAME", "Lane's server")
	fs, target, keep, display := newTestFlags()
	if _, err := parseArgs(fs, []string{"valheim"}, true); err != nil {
		t.Fatal(err)
	}
	if *target != "valheim-server" || !*keep || *display != "Lane's server" {
		t.Errorf("from env: target %q keep %v display %q", *target, *keep, *display)
	}

	// The command line wins over the environment.
	fs, target, keep, _ = newTestFlags()
	if _, err := parseArgs(fs, []string{"--target", "10.0.0.5", "--keep=false"}, true); err != nil {
		t.Fatal(err)
	}
	if *target != "10.0.0.5" || *keep {
		t.Errorf("flags didn't override env: target %q keep %v", *target, *keep)
	}

	// Without env, the environment is ignored.
	fs, target, _, _ = newTestFlags()
	parseArgs(fs, nil, false)
	if *target != "127.0.0.1" {
		t.Errorf("env read when it shouldn't be: %q", *target)
	}

	t.Setenv("TAILLAN_KEEP", "maybe")
	fs, _, _, _ = newTestFlags()
	if _, err := parseArgs(fs, nil, true); err == nil || !strings.Contains(err.Error(), "TAILLAN_KEEP") {
		t.Errorf("bad env value: err = %v", err)
	}
}

func TestEnvArg(t *testing.T) {
	t.Setenv("TAILLAN_GAME", " valheim ")
	if g := envArg(nil, "TAILLAN_GAME"); g != "valheim" {
		t.Errorf("from env: %q", g)
	}
	if g := envArg([]string{"7777/udp", "27015/tcp"}, "TAILLAN_GAME"); g != "7777/udp 27015/tcp" {
		t.Errorf("from args: %q", g)
	}
}

func TestParseTargets(t *testing.T) {
	got, err := parseTargets("25566", 1)
	if err != nil || got[0] != 25566 || len(got) != 1 {
		t.Errorf("one port: %v, %v", got, err)
	}
	got, err = parseTargets(" ,8889", 2)
	if err != nil || got[1] != 8889 || len(got) != 1 {
		t.Errorf("second group only: %v, %v", got, err)
	}
	for _, bad := range []string{"0", "70000", "abc", "1,2,3"} {
		if _, err := parseTargets(bad, 2); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMatchFriend(t *testing.T) {
	friends := []session.FriendStatus{
		{ID: "nodekey:aaaa1111", Name: "Sam"},
		{ID: "nodekey:bbbb2222", Name: "Alex"},
		{ID: "nodekey:bbbb3333", Name: "Alex"},
	}
	if f, err := matchFriend(friends, "sam"); err != nil || f.Name != "Sam" {
		t.Errorf("by name: %v, %v", f, err)
	}
	if f, err := matchFriend(friends, "bbbb3"); err != nil || f.ID != "nodekey:bbbb3333" {
		t.Errorf("by id prefix: %v, %v", f, err)
	}
	if f, err := matchFriend(friends, "nodekey:aaaa1111"); err != nil || f.Name != "Sam" {
		t.Errorf("by full id: %v, %v", f, err)
	}
	if _, err := matchFriend(friends, "alex"); err == nil || !strings.Contains(err.Error(), "use the id") {
		t.Errorf("two friends called Alex: %v", err)
	}
	if _, err := matchFriend(friends, "bbb"); err == nil {
		t.Error("a 3-character id prefix matched")
	}
	if _, err := matchFriend(friends, "Jo"); err == nil || !strings.Contains(err.Error(), "Sam") {
		t.Errorf("unknown friend: %v", err)
	}
	if _, err := matchFriend(nil, "Jo"); err == nil {
		t.Error("matched in an empty room")
	}
}

func TestTerminalSteps(t *testing.T) {
	got := terminalSteps([]string{"Run the server.", "Click Start sharing and send the invite code to your friends."})
	if len(got) != 1 || got[0] != "Run the server." {
		t.Errorf("got %q", got)
	}
	if got := terminalSteps([]string{"Run the server."}); len(got) != 1 {
		t.Errorf("dropped a real step: %q", got)
	}
}

func TestFmtDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		12 * time.Second:               "12s",
		12*time.Minute + 3*time.Second: "12m",
		2*time.Hour + 13*time.Minute:   "2h13m",
	} {
		if got := fmtDuration(d); got != want {
			t.Errorf("fmtDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestInviteMessage(t *testing.T) {
	m := inviteMessage("Valheim", "TL1-abc", "https://example.com/dl")
	for _, want := range []string{"Join my Valheim game", "https://example.com/dl", "\nTL1-abc\n", "private"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
}
