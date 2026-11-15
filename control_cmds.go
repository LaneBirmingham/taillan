package main

// Commands that talk to a TailLAN that's already running: the app, or
// "taillan host" / "taillan join" in another terminal, a service or a
// container. See internal/control.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/LaneBirmingham/taillan/internal/config"
	"github.com/LaneBirmingham/taillan/internal/control"
	"github.com/LaneBirmingham/taillan/internal/profile"
	"github.com/LaneBirmingham/taillan/internal/session"
)

// exitCode ends taillan with a status and no message, for scripts and
// health checks ("taillan status" exits 1 when nothing is running).
type exitCode int

func (e exitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// running is one running TailLAN and what it's doing.
type running struct {
	control.Instance
	State *control.State
}

// starting reports whether a "taillan host" or "taillan join" hasn't
// begun its session yet: it may be waiting for the network.
func (r running) starting() bool {
	return r.Kind != "app" && r.State.Host == nil && r.State.Join == nil
}

// gameID is the game a session shares or joins, or is about to.
func (r running) gameID() string {
	if id := r.State.GameID(); id != "" {
		return id
	}
	return r.Game
}

func (r running) label() string {
	switch {
	case r.starting():
		return fmt.Sprintf("starting %s (PID %d)", labelOr(r.Game, "a game"), r.PID)
	case r.State.Host != nil:
		return fmt.Sprintf("sharing %s (PID %d)", r.State.Host.Game.ID, r.PID)
	case r.State.Join != nil:
		return fmt.Sprintf("joined %s (PID %d)", r.State.Join.Game.ID, r.PID)
	}
	return fmt.Sprintf("the app, idle (PID %d)", r.PID)
}

// listRunning returns every running TailLAN with its state.
func listRunning(ctx context.Context) []running {
	var out []running
	for _, in := range control.Running(ctx) {
		st, err := in.State(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Skipping TailLAN PID %d: %v\n", in.PID, err)
			continue
		}
		out = append(out, running{in, st})
	}
	return out
}

// matches reports whether a session fits a --game selector: a game id
// (or a unique search for one), or the process ID.
func (r running) matches(sel string) bool {
	if sel == "" {
		return true
	}
	if strconv.Itoa(r.PID) == sel {
		return true
	}
	id := r.gameID()
	return id != "" && (id == sel || id == profile.Slug(sel))
}

// pickSession finds the one active session a command acts on. want
// narrows it to hosting ("host") or any session (""). Sessions still
// starting are included only if starting is true (for "stop").
func pickSession(ctx context.Context, sel, want string, starting bool) (running, error) {
	all := listRunning(ctx)
	if len(all) == 0 {
		return running{}, errors.New(`TailLAN isn't running. Start sharing with "taillan host <game>"`)
	}
	var found, waiting []running
	for _, r := range all {
		if !r.matches(sel) {
			continue
		}
		if r.starting() {
			if starting {
				found = append(found, r)
			} else if want == "" || r.Kind == "host" {
				waiting = append(waiting, r)
			}
			continue
		}
		if !r.State.Active() {
			continue
		}
		if want == "host" && r.State.Host == nil {
			continue
		}
		found = append(found, r)
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		var names []string
		for _, r := range found {
			names = append(names, r.label())
		}
		return running{}, fmt.Errorf("several games are running: %s. Say which with --game <id>", strings.Join(names, "; "))
	case len(waiting) > 0:
		return running{}, fmt.Errorf(`%s is still starting (it may be waiting for the network). Try again once "taillan status" shows it sharing`, labelOr(waiting[0].Game, "TailLAN"))
	}
	what := "shared or joined"
	if want == "host" {
		what = "shared"
	}
	if sel != "" {
		var names []string
		for _, r := range all {
			names = append(names, r.label())
		}
		return running{}, fmt.Errorf("no game matching %q is being %s. Running: %s", sel, what, strings.Join(names, "; "))
	}
	return running{}, fmt.Errorf(`no game is being %s right now. Start with "taillan host <game>"`, what)
}

// sessionFlags adds the --game selector, and reads it or the first
// argument.
func sessionFlags(name, synopsis, note string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	sel := fs.String("game", "", "which session, if several are running: a game id or a PID")
	fs.Usage = commandUsage(fs, synopsis, note, false)
	return fs, sel
}

func selector(sel string, pos []string) string {
	if sel == "" && len(pos) > 0 {
		return pos[0]
	}
	return sel
}

// --- status ---

func runStatus(args []string) error {
	fs, sel := sessionFlags("status", "status [game]", "")
	asJSON := fs.Bool("json", false, "print the state as JSON (it includes invite codes)")
	quiet := fs.Bool("quiet", false, "print nothing, just set the exit status (for health checks)")
	fs.Usage = commandUsage(fs, "status [game]", "Shows what's being shared or joined. Exits 0 while a game is shared, or joined\nand connected; 1 otherwise.", false)
	pos, _ := parseArgs(fs, args, false)
	*sel = selector(*sel, pos)
	ctx := context.Background()
	var list []running
	for _, r := range listRunning(ctx) {
		if r.matches(*sel) {
			list = append(list, r)
		}
	}
	healthy := false
	for _, r := range list {
		if r.State.Host != nil || (r.State.Join != nil && r.State.Join.Phase == "connected") {
			healthy = true
		}
	}
	switch {
	case *quiet:
	case *asJSON:
		type entry struct {
			PID      int             `json:"pid"`
			Kind     string          `json:"kind"`
			Started  time.Time       `json:"started"`
			Version  string          `json:"version"`
			Name     string          `json:"name"`
			Starting bool            `json:"starting,omitempty"`
			Game     string          `json:"game,omitempty"`
			Session  json.RawMessage `json:"session"`
		}
		out := []entry{}
		for _, r := range list {
			out = append(out, entry{r.PID, r.Kind, r.Started, r.State.Version, r.State.Name, r.starting(), r.gameID(), r.State.Session})
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	case len(list) == 0 && *sel != "":
		fmt.Printf("Nothing matching %q is running.\n", *sel)
	case len(list) == 0:
		fmt.Println(`TailLAN isn't running. Share a game with "taillan host <game>", or open the app with "taillan".`)
	default:
		for i, r := range list {
			if i > 0 {
				fmt.Println()
			}
			printStatus(r)
		}
	}
	if !healthy {
		return exitCode(1)
	}
	return nil
}

func kindLabel(k string) string {
	switch k {
	case "app":
		return "the app"
	case "host":
		return "taillan host"
	case "join":
		return "taillan join"
	}
	return k
}

func printStatus(r running) {
	since := fmtDuration(time.Since(r.Started))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	switch st := r.State; {
	case r.starting():
		fmt.Printf("Starting %s (%s, PID %d, for %s): not sharing yet. If this lasts, check its output; it may be waiting for the network.\n", labelOr(r.Game, "a game"), kindLabel(r.Kind), r.PID, since)
	case st.Host != nil:
		h := st.Host
		fmt.Printf("Sharing %s (%s, PID %d, running %s)\n", h.Game.Name, kindLabel(r.Kind), r.PID, since)
		room := "open: anyone with the invite can join"
		if h.Locked {
			room = `locked: only friends already here can reconnect ("taillan unlock" opens it)`
		}
		fmt.Fprintf(w, "  Room:\t%s\n", room)
		code := "new for this session"
		if h.KeepCode {
			code = "kept: the same code every time"
		}
		fmt.Fprintf(w, "  Invite:\t%s (\"taillan invite\" prints it)\n", code)
		for i, g := range h.Groups {
			label := "Server:"
			if i > 0 {
				label = ""
			}
			fmt.Fprintf(w, "  %s\t%s %s/%s → %s:%d, %s\n", label, labelOr(g.Label, "Game"), g.Ports, g.Proto, h.TargetIP, g.Target, listenWords(g))
		}
		online := 0
		for _, f := range h.Friends {
			if f.Online {
				online++
			}
		}
		fmt.Fprintf(w, "  Friends:\t%d online\n", online)
		for _, f := range h.Friends {
			if !f.Online {
				continue
			}
			fmt.Fprintf(w, "  \t  %s\t%s\n", f.Name, pathWords(f.Path, f.Path.LatencyMs))
		}
	case st.Join != nil:
		j := st.Join
		fmt.Printf("Joined %s's %s (%s, PID %d, running %s)\n", hostOr(j.HostName), j.Game.Name, kindLabel(r.Kind), r.PID, since)
		state := j.Phase
		if j.Phase == "connected" {
			state += ", " + pathWords(j.Path, j.RTTMs)
		} else if j.Message != "" {
			state += ": " + j.Message
		}
		fmt.Fprintf(w, "  Status:\t%s\n", state)
		fmt.Fprintf(w, "  Connect to:\t%s\n", j.Connect)
		for i, p := range j.Ports {
			label := "Ports:"
			if i > 0 {
				label = ""
			}
			fmt.Fprintf(w, "  %s\t%s → %s/%s on the host\n", label, p.Local, p.Remote, p.Proto)
		}
		for _, n := range j.Notes {
			fmt.Fprintf(w, "  Note:\t%s\n", n)
		}
		for _, n := range j.Warnings {
			fmt.Fprintf(w, "  Warning:\t%s\n", n)
		}
	default:
		fmt.Printf("The TailLAN app is open (PID %d) but isn't sharing or joining a game.\n", r.PID)
	}
}

func labelOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func listenWords(g session.GroupStatus) string {
	switch g.Listening {
	case "yes":
		if g.Process != "" {
			return "server running (" + g.Process + ")"
		}
		return "server running"
	case "partial":
		return "server partly running (some ports aren't open yet)"
	case "no":
		return "server NOT running"
	}
	return "can't tell if the server is running"
}

func pathWords(p session.PathInfo, ms float64) string {
	out := "finding the best route"
	switch p.Kind {
	case "direct":
		out = "direct"
	case "relay":
		out = "relayed via " + p.Via
	}
	if ms > 0 {
		out += fmt.Sprintf(", %.0f ms", ms)
	}
	return out
}

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// --- invite ---

func runInvite(args []string) error {
	fs, sel := sessionFlags("invite", "invite [game]", "Prints the invite code of the game you're sharing. Send it privately: it's the key.")
	message := fs.Bool("message", false, "print a ready-to-send message for friends, not just the code")
	pos, _ := parseArgs(fs, args, false)
	r, err := pickSession(context.Background(), selector(*sel, pos), "host", false)
	if err != nil {
		return err
	}
	h := r.State.Host
	if !*message {
		fmt.Println(h.Invite)
		return nil
	}
	fmt.Println(inviteMessage(h.Game.Name, h.Invite, r.State.Download))
	return nil
}

// inviteMessage is the text friends get, as the app's "Copy invite
// message" button makes it.
func inviteMessage(game, code, download string) string {
	get := "1. Open TailLAN (free, one small app)."
	if download != "" {
		get = "1. Get TailLAN (free, one small app): " + download
	}
	return strings.Join([]string{
		"Join my " + game + " game with TailLAN!",
		get,
		"2. Click Join a game and paste this code:",
		"",
		code,
		"",
		"(Keep this code private: anyone with it can join.)",
	}, "\n")
}

// --- lock / unlock / kick ---

func runLock(args []string, locked bool) error {
	name := "lock"
	if !locked {
		name = "unlock"
	}
	fs, sel := sessionFlags(name, name+" [game]", "Locking stops new friends joining with the invite; friends already here can\nreconnect. Unlocking lets anyone with the invite in again.")
	pos, _ := parseArgs(fs, args, false)
	ctx := context.Background()
	r, err := pickSession(ctx, selector(*sel, pos), "host", false)
	if err != nil {
		return err
	}
	if err := r.Post(ctx, "/api/host/lock", map[string]bool{"locked": locked}); err != nil {
		return err
	}
	if locked {
		fmt.Println("Room locked: only friends already here can reconnect.")
	} else {
		fmt.Println("Room unlocked: anyone with the invite can join.")
	}
	return nil
}

func runKick(args []string) error {
	fs, sel := sessionFlags("kick", "kick <friend name or id>", "Removes a friend and locks the room, since anyone with the invite could come\nback under a new identity. \"taillan status\" lists who's here.")
	pos, _ := parseArgs(fs, args, false)
	who := strings.TrimSpace(strings.Join(pos, " "))
	if who == "" {
		fs.Usage()
		return errors.New(`who? Give a friend's name, as "taillan status" lists them`)
	}
	ctx := context.Background()
	r, err := pickSession(ctx, *sel, "host", false)
	if err != nil {
		return err
	}
	f, err := matchFriend(r.State.Host.Friends, who)
	if err != nil {
		return err
	}
	if err := r.Post(ctx, "/api/host/kick", map[string]string{"id": f.ID}); err != nil {
		return err
	}
	fmt.Printf("Removed %s. The room is now locked, so they can't come back with the invite; \"taillan unlock\" lets new friends in again.\n", f.Name)
	return nil
}

// matchFriend finds a friend by name (any case) or by id (or the start
// of one, at least 4 characters).
func matchFriend(friends []session.FriendStatus, q string) (session.FriendStatus, error) {
	strip := func(s string) string { return strings.TrimPrefix(strings.ToLower(s), "nodekey:") }
	var byName, byID []session.FriendStatus
	for _, f := range friends {
		switch {
		case f.ID == q:
			return f, nil
		case strings.EqualFold(f.Name, q):
			byName = append(byName, f)
		case len(strip(q)) >= 4 && strings.HasPrefix(strip(f.ID), strip(q)):
			byID = append(byID, f)
		}
	}
	found := byName
	if len(found) == 0 {
		found = byID
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		var names []string
		for _, f := range friends {
			names = append(names, f.Name)
		}
		if len(names) == 0 {
			return session.FriendStatus{}, errors.New("nobody has joined yet")
		}
		return session.FriendStatus{}, fmt.Errorf("no friend called %q. Here: %s", q, strings.Join(names, ", "))
	}
	var ids []string
	for _, f := range found {
		id := strip(f.ID)
		if len(id) > 8 {
			id = id[:8]
		}
		ids = append(ids, f.Name+" ("+id+")")
	}
	return session.FriendStatus{}, fmt.Errorf("%q could be any of %s; use the id instead", q, strings.Join(ids, ", "))
}

// --- stop ---

func runStop(args []string) error {
	fs, sel := sessionFlags("stop", "stop [game]", "Stops sharing, or leaves the game you joined.")
	all := fs.Bool("all", false, "stop every game being shared or joined")
	pos, _ := parseArgs(fs, args, false)
	ctx := context.Background()
	var targets []running
	if *all {
		for _, r := range listRunning(ctx) {
			if r.State.Active() || r.starting() {
				targets = append(targets, r)
			}
		}
		if len(targets) == 0 {
			fmt.Println("Nothing is being shared or joined.")
			return nil
		}
	} else {
		r, err := pickSession(ctx, selector(*sel, pos), "", true)
		if err != nil {
			return err
		}
		targets = []running{r}
	}
	var errs []error
	for _, r := range targets {
		if r.starting() {
			// Not sharing yet: quit it, so it stops trying.
			if err := r.Post(ctx, "/api/quit", nil); err != nil {
				errs = append(errs, err)
				continue
			}
			fmt.Printf("Stopped %s before it started sharing.\n", labelOr(r.Game, "TailLAN"))
		} else if h := r.State.Host; h != nil {
			if err := r.Post(ctx, "/api/host/stop", nil); err != nil {
				errs = append(errs, err)
				continue
			}
			fmt.Printf("Stopped sharing %s.\n", h.Game.Name)
		} else if j := r.State.Join; j != nil {
			if err := r.Post(ctx, "/api/join/stop", nil); err != nil {
				errs = append(errs, err)
				continue
			}
			fmt.Printf("Left %s's %s.\n", hostOr(j.HostName), j.Game.Name)
		}
	}
	return errors.Join(errs...)
}

// --- name / retire ---

func runName(args []string) error {
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		return errors.New(`usage: taillan name [new name]   (prints your name, or changes it)`)
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	n := config.CleanName(strings.Join(args, " "))
	if n == "" {
		fmt.Println(settings.Name)
		return nil
	}
	settings.Name = n
	if err := config.Save(settings); err != nil {
		return err
	}
	// A running TailLAN keeps its settings in memory: tell it too, so
	// it doesn't later save the old name back.
	ctx := context.Background()
	for _, in := range control.Running(ctx) {
		in.Post(ctx, "/api/settings", map[string]string{"name": n})
	}
	fmt.Printf("Friends will see you as %q from the next game you host or join.\n", n)
	return nil
}

func runRetire(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(`usage: taillan retire <game>   (the game you shared with --keep)`)
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	// The key is named after the game id, which for games shared by
	// port numbers is a slug of their name: try that exactly first, so
	// a search can't pick a different game.
	id, name := profile.Slug(args[0]), args[0]
	hk, err := config.LoadHostKey("game-" + id)
	if err != nil {
		return err
	}
	if hk == nil {
		if p := store.Get(args[0]); p != nil {
			id, name = p.ID, p.Name
		} else if m := store.Find(args[0]); len(m) == 1 {
			id, name = m[0].ID, m[0].Name
		}
		if hk, err = config.LoadHostKey("game-" + id); err != nil {
			return err
		}
	} else if p := store.Get(id); p != nil {
		name = p.Name
	}
	if hk == nil {
		return fmt.Errorf("there's no kept invite for %s", name)
	}
	if err := config.DeleteHostKey("game-" + id); err != nil {
		return err
	}
	fmt.Printf("Retired the kept invite for %s: the old code stops working once the current session (if any) ends. Next time you share it with --keep, it gets a new code.\n", name)
	return nil
}
