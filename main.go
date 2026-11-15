// TailLAN shares a multiplayer game with friends over tailcat: an
// encrypted, peer-to-peer tunnel that needs no accounts, no port
// forwarding and no admin rights.
//
// Run it with no arguments for the app window. Subcommands offer the
// same features from a terminal, plus tools for adding games.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/LaneBirmingham/taillan/internal/config"
	"github.com/LaneBirmingham/taillan/internal/control"
	"github.com/LaneBirmingham/taillan/internal/doctor"
	"github.com/LaneBirmingham/taillan/internal/helpers"
	"github.com/LaneBirmingham/taillan/internal/invite"
	"github.com/LaneBirmingham/taillan/internal/launch"
	"github.com/LaneBirmingham/taillan/internal/portscan"
	"github.com/LaneBirmingham/taillan/internal/profile"
	"github.com/LaneBirmingham/taillan/internal/session"
	"github.com/LaneBirmingham/taillan/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

// The licenses of TailLAN and of everything compiled into it, which
// binary releases must carry. Regenerate the notices with `make notices`.
var (
	//go:embed LICENSE
	licenseText string
	//go:embed THIRD_PARTY_NOTICES.txt
	thirdPartyNotices string
)

const usage = `TailLAN: play multiplayer games with friends, no port forwarding needed.

Usage:
  taillan                      open the app (same as "taillan ui")
  taillan ui [--no-open]       run the app; --no-open just prints its URL
  taillan host <game|ports>    share a game from the terminal (servers, SSH, containers too)
  taillan join <invite>        join a friend's game from the terminal

While sharing or joining (from the app, a terminal, a service or a container):
  taillan status [--json]      what's running: the game, the room, friends, the connection
  taillan invite [--message]   print the invite code of the game you're sharing
  taillan lock | unlock        stop new friends joining with the invite, or allow them again
  taillan kick <friend>        remove a friend (by name); this also locks the room
  taillan stop [--all]         stop sharing, or leave the game you joined

Settings and games:
  taillan name [new name]      show or change the name friends see
  taillan retire <game>        retire a kept invite code (host --keep) so it stops working
  taillan games [search]       list known games
  taillan game <command>       add, check and inspect game profiles:
      new                        create a profile (asks a few questions)
      validate <file.json>...    check profile files for mistakes
      show <id>                  print a profile's JSON
      dir                        print (and create) your games folder
  taillan detect [--watch]     show which ports running programs listen on
  taillan doctor               check this network for hosting and joining
  taillan helpers              list built-in game helpers
  taillan licenses             show the licenses of TailLAN and its components
  taillan version

Run "taillan <command> -h" for its flags. The flags of ui, host and join can
also be set with environment variables: TAILLAN_ and the flag name in capitals,
such as TAILLAN_KEEP=1 or TAILLAN_DISPLAY_NAME="Lane's server".

Examples:
  taillan host minecraft-java
  taillan host valheim --keep --display-name "Lane's server"
  taillan host "7777/udp, 27015/tcp" --name "My Game"
  taillan host minecraft-java --target mc-server --port 25566
  taillan join TL1-...

Servers and containers: https://github.com/LaneBirmingham/taillan/blob/main/docs/server.md
`

func main() {
	session.Version = version
	args := os.Args[1:]
	cmd := "ui"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	// Pasting an invite as the only argument means "join".
	if strings.HasPrefix(cmd, invite.Prefix) {
		cmd, args = "join", append([]string{cmd}, args...)
	}
	var err error
	switch cmd {
	case "ui", "app":
		err = runUI(args)
	case "host", "share":
		err = runHost(args)
	case "join":
		err = runJoin(args)
	case "games", "list":
		err = runGames(args)
	case "game", "profile":
		err = runGame(args)
	case "detect", "ports":
		err = runDetect(args)
	case "status":
		err = runStatus(args)
	case "invite":
		err = runInvite(args)
	case "lock":
		err = runLock(args, true)
	case "unlock":
		err = runLock(args, false)
	case "kick", "remove":
		err = runKick(args)
	case "stop", "leave":
		err = runStop(args)
	case "name":
		err = runName(args)
	case "retire":
		err = runRetire(args)
	case "doctor":
		err = runDoctor()
	case "helpers":
		for _, h := range helpers.List() {
			fmt.Printf("%-16s %s\n", h.Name, h.Description)
		}
	case "version", "--version", "-v":
		fmt.Println("taillan", version)
	case "licenses", "license":
		fmt.Print(licenseText, "\n", thirdPartyNotices)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	var code exitCode
	if errors.As(err, &code) {
		os.Exit(int(code))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func openStore() (*profile.Store, error) {
	return profile.NewStore(config.GamesDir())
}

// interrupt returns a context that ends on Ctrl+C, on SIGTERM (from a
// service manager or "docker stop"), and when the terminal or SSH
// session closes (SIGHUP), so friends hear that the game ended. Under
// nohup, which asks to survive the terminal closing, SIGHUP is left
// ignored.
func interrupt() (context.Context, context.CancelFunc) {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return signal.NotifyContext(context.Background(), sigs...)
}

// serveControl serves app's API on a random local port and registers
// it, so other taillan commands (status, kick, stop...) can drive this
// session. The returned function unregisters it and stops the server,
// letting a request in flight (such as the "stop" that ended the
// session) finish first.
func serveControl(app *ui.App, kind string) func() {
	ln, err := app.ListenAddr("127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: other taillan commands won't see this session (%v)\n", err)
		return func() {}
	}
	srv := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	unregister, err := control.Register(control.Instance{URL: app.URL, Token: app.Token, PID: os.Getpid(), Kind: kind, Started: time.Now()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: other taillan commands won't see this session (%v)\n", err)
		unregister = func() {}
	}
	return func() {
		unregister()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
}

// --- ui ---

// existingInstance returns the launch URL of an already-running
// TailLAN app, so a second launch just brings up its window.
func existingInstance() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, in := range control.Running(ctx) {
		if in.Kind == "app" {
			return in.URL + "/?t=" + in.Token
		}
	}
	return ""
}

func runUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	noOpen := fs.Bool("no-open", false, "don't open a window; just print the URL")
	stay := fs.Bool("stay", false, "keep running when the window is closed and nothing is shared")
	fs.Usage = commandUsage(fs, "ui", "Runs the app. With --no-open --stay it keeps running headless; reach its window\nover SSH with: ssh -L 47800:127.0.0.1:47800 <server>, then open the URL it prints.", true)
	if _, err := parseArgs(fs, args, true); err != nil {
		return err
	}

	if u := existingInstance(); u != "" {
		fmt.Println("TailLAN is already running; opening its window.")
		if !*noOpen {
			return launch.OpenApp(u)
		}
		fmt.Println(u)
		return nil
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	log := session.NewLogBuffer(func(s string) { fmt.Println(s) })
	app := ui.NewApp(store, log, settings)
	ln, err := app.Listen()
	if err != nil {
		return err
	}
	ctx, stop := interrupt()
	defer stop()
	app.Quit = stop

	go app.Serve(ctx, ln)
	if unregister, err := control.Register(control.Instance{URL: app.URL, Token: app.Token, PID: os.Getpid(), Kind: "app", Started: time.Now()}); err == nil {
		defer unregister()
	}
	fmt.Printf("TailLAN %s is running.\n", version)
	fmt.Printf("  Window: %s\n", app.LaunchURL())
	fmt.Println("  Close this terminal (or press Ctrl+C) to quit. Closing the window keeps any game you're sharing running.")
	if !*noOpen {
		if err := launch.OpenApp(app.LaunchURL()); err != nil {
			fmt.Println("  Couldn't open a window automatically; open the link above in your browser.")
		}
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("Shutting down...")
			app.Shutdown()
			return nil
		case <-tick.C:
			if !*stay && app.Idle(2*time.Minute) {
				fmt.Println("Window closed and nothing shared; exiting. Run TailLAN again any time.")
				app.Shutdown()
				return nil
			}
		}
	}
}

// --- host ---

func runHost(args []string) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	name := fs.String("name", "", "game name, when sharing by port numbers")
	extra := fs.String("extra", "", "extra ports to share too, e.g. \"64738/both\"")
	target := fs.String("target", "127.0.0.1", "where the game server runs: this machine, another device's IP, or a host name\n(such as a container's service name)")
	port := fs.String("port", "", "where the game listens, if not on its usual port: \"7777\", or one per port group: \"7777,8888\"")
	keep := fs.Bool("keep", false, "keep the same invite code for next time (the key is saved in your config folder)")
	display := fs.String("display-name", "", "the name friends see (default: your saved name, see \"taillan name\")")
	inviteFile := fs.String("invite-file", "", "write the invite code to this file while sharing (removed when sharing stops)")
	printInvite := fs.Bool("print-invite", true, "print the invite code; set to false if others can read your logs,\nand get it with \"taillan invite\" or --invite-file")
	fs.Usage = commandUsage(fs, "host <game-id | ports>", "Shares a game until Ctrl+C, SIGTERM or \"taillan stop\". The game can also be set\nwith TAILLAN_GAME.", true)
	pos, err := parseArgs(fs, args, true)
	if err != nil {
		return err
	}
	game := envArg(pos, "TAILLAN_GAME")
	if game == "" {
		fs.Usage()
		return errors.New("which game? Run \"taillan games\" to list them, or give ports like \"7777/udp\"")
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	if n := config.CleanName(*display); n != "" {
		settings.Name = n
	}
	log := session.NewLogBuffer(func(s string) { fmt.Println(s) })
	app := ui.NewApp(store, log, settings)
	app.Headless = true
	p, err := hostProfile(app, store, game, *name, *extra)
	if err != nil {
		return err
	}
	targets, err := parseTargets(*port, len(p.Ports))
	if err != nil {
		return err
	}
	if _, err := session.NormalizeTarget(*target); err != nil {
		return err
	}
	ctx, stop := interrupt()
	defer stop()
	app.Quit = stop
	// Two sessions with one kept invite would be one identity in two
	// places, and fight over it.
	if *keep {
		for _, r := range listRunning(ctx) {
			if h := r.State.Host; h != nil && h.KeepCode && r.State.GameID() == p.ID {
				return fmt.Errorf("%s is already being shared with its kept invite (PID %d). Stop that first (taillan stop %d), or share this copy without --keep", p.Name, r.PID, r.PID)
			}
		}
	}
	defer serveControl(app, "host")()

	if steps := terminalSteps(p.Host.Steps); len(steps) > 0 {
		fmt.Printf("\nHosting %s:\n", p.Name)
		for i, s := range steps {
			fmt.Printf("  %d. %s\n", i+1, s)
		}
		fmt.Println()
	}
	h, err := startHostRetrying(ctx, app, session.HostConfig{
		Profile: p, TargetIP: *target, Targets: targets, KeepCode: *keep,
		HostName: settings.Name, Verbose: settings.Verbose,
	})
	if h == nil || err != nil {
		return err
	}
	defer app.Shutdown()
	if *inviteFile != "" {
		if err := writeInviteFile(*inviteFile, h.Invite()); err != nil {
			return fmt.Errorf("couldn't write the invite file: %v", err)
		}
		defer os.Remove(*inviteFile)
	}
	if *printInvite {
		fmt.Printf("\nSend this invite code to your friends (it's the key to your game; share it privately):\n\n  %s\n\n", h.Invite())
		fmt.Println("They paste it into TailLAN's Join screen, or run: taillan join <code>")
	} else {
		fmt.Println("\nSharing. Get the invite code with \"taillan invite\" and send it to your friends privately.")
	}
	if *inviteFile != "" {
		fmt.Println("Invite code saved to", *inviteFile)
	}
	fmt.Println("From another terminal: taillan status, taillan lock, taillan kick <name>, taillan stop.")
	fmt.Println("Press Ctrl+C to stop sharing.")
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.Done():
			return nil
		case <-t.C:
			st := h.Status()
			var parts []string
			for _, g := range st.Groups {
				parts = append(parts, fmt.Sprintf("port %d: server %s", g.Target, g.Listening))
			}
			online := 0
			for _, f := range st.Friends {
				if f.Online {
					online++
					parts = append(parts, fmt.Sprintf("%s (%s %s)", f.Name, f.Path.Kind, fmtMs(f.Path.LatencyMs)))
				}
			}
			room := ""
			if st.Locked {
				room = ", room locked"
			}
			line := fmt.Sprintf("[%d friend(s)%s] %s", online, room, strings.Join(parts, " | "))
			if line != last {
				fmt.Println(line)
				last = line
			}
		}
	}
}

// hostProfile finds the game to share: a game id, a search that finds
// exactly one game, or a list of ports.
func hostProfile(app *ui.App, store *profile.Store, game, name, extra string) (*profile.Profile, error) {
	id, spec := game, ""
	if store.Get(game) == nil {
		if matches := store.Find(game); len(matches) == 1 {
			id = matches[0].ID
		} else if _, perr := profile.ParsePorts(game); perr == nil {
			id, spec = "", game
		} else if len(matches) > 1 {
			var ids []string
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			return nil, fmt.Errorf("%q matches several games: %s", game, strings.Join(ids, ", "))
		} else {
			return nil, fmt.Errorf("no game %q; run \"taillan games\", or give ports like \"7777/udp\"", game)
		}
	}
	return app.ResolveGame(id, name, spec, extra)
}

// terminalSteps are a game's host steps without the last one, "Click
// Start sharing...", which is about the app's button.
func terminalSteps(steps []string) []string {
	if n := len(steps); n > 0 && strings.HasPrefix(steps[n-1], "Click Start sharing") {
		return steps[:n-1]
	}
	return steps
}

// startHostRetrying starts sharing, and if the network isn't there
// yet (a server that just booted, Wi-Fi that dropped), keeps trying
// until it is or ctx ends. It returns nil, nil if ctx ended first.
func startHostRetrying(ctx context.Context, app *ui.App, cfg session.HostConfig) (*session.Host, error) {
	for attempt := 1; ; attempt++ {
		h, err := app.StartHosting(ctx, cfg)
		if err == nil {
			return h, nil
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		if !session.Retryable(err) {
			return nil, err
		}
		wait := time.Duration(min(attempt*5, 60)) * time.Second
		fmt.Printf("Trying again in %s (press Ctrl+C to give up)...\n", wait)
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(wait):
		}
	}
}

// writeInviteFile saves the invite code where only this user can read
// it: it's the key to the game.
func writeInviteFile(path, code string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(code+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func fmtMs(ms float64) string {
	if ms <= 0 {
		return ""
	}
	return fmt.Sprintf("%.0fms", ms)
}

// --- join ---

func runJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	allowLAN := fs.Bool("allow-lan", false, "let other devices on your LAN use the forwarded ports the game marks for LAN play")
	display := fs.String("display-name", "", "the name the host sees (default: your saved name, see \"taillan name\")")
	fs.Usage = commandUsage(fs, "join <invite>", "Joins until Ctrl+C, SIGTERM, \"taillan stop\" or the host stops. The invite can\nalso be set with TAILLAN_INVITE.", true)
	pos, err := parseArgs(fs, args, true)
	if err != nil {
		return err
	}
	code := envArg(pos, "TAILLAN_INVITE")
	if code == "" {
		if !interactive() {
			return errors.New("no invite code: give it as an argument (taillan join TL1-...) or in TAILLAN_INVITE")
		}
		fmt.Print("Paste the invite code: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		code = strings.TrimSpace(line)
	}
	inv, err := invite.Decode(code)
	if err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	if n := config.CleanName(*display); n != "" {
		settings.Name = n
	}
	p, err := session.JoinProfile(inv, store.Builtin(inv.GameID))
	if err != nil {
		return err
	}
	log := session.NewLogBuffer(func(s string) { fmt.Println(s) })
	app := ui.NewApp(store, log, settings)
	app.Headless = true
	ctx, stop := interrupt()
	defer stop()
	app.Quit = stop
	defer serveControl(app, "join")()
	j, err := app.StartJoining(session.JoinConfig{
		Invite: inv, Profile: p, Name: settings.Name, Key: settings.ClientPrivateKey(),
		AllowLAN: *allowLAN, Verbose: settings.Verbose,
	})
	if err != nil {
		return err
	}
	defer app.Shutdown()
	st := j.Status()
	fmt.Printf("\nJoining %s's %s. In the game:\n", hostOr(inv.HostName), p.Name)
	for i, s := range st.Steps {
		fmt.Printf("  %d. %s\n", i+1, s)
	}
	fmt.Printf("\n  Connect to: %s\n\nPress Ctrl+C to leave.\n", st.Connect)
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-j.Done():
			return nil
		case <-t.C:
			st := j.Status()
			line := st.Phase
			if st.Path.Kind != "" {
				line += fmt.Sprintf(", %s %s", st.Path.Kind, fmtMs(st.RTTMs))
			}
			if line != last {
				fmt.Println("Status:", line)
				last = line
			}
		}
	}
}

func hostOr(n string) string {
	if n == "" {
		return "your friend"
	}
	return n
}

// --- games / game ---

func runGames(args []string) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	q := strings.Join(args, " ")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tGAME\tPORTS\tSOURCE")
	for _, p := range store.Find(q) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ID, p.Name, p.PortSummary(), p.Source)
	}
	w.Flush()
	for _, pr := range store.Problems() {
		fmt.Fprintln(os.Stderr, "Skipped broken game file:", pr)
	}
	fmt.Printf("\nYour own games live in %s\n", store.Dir())
	return nil
}

func runGame(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: taillan game new|validate|show|dir")
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "dir":
		os.MkdirAll(store.Dir(), 0o755)
		fmt.Println(store.Dir())
	case "show":
		if len(args) < 2 {
			return errors.New("usage: taillan game show <id>")
		}
		p := store.Get(args[1])
		if p == nil {
			return fmt.Errorf("no game %q", args[1])
		}
		b, _ := p.Marshal()
		os.Stdout.Write(b)
	case "validate":
		if len(args) < 2 {
			return errors.New("usage: taillan game validate <file.json>...")
		}
		bad := 0
		for _, f := range args[1:] {
			data, err := os.ReadFile(f)
			if err == nil {
				var p *profile.Profile
				p, err = profile.Parse(data)
				if err == nil {
					fmt.Printf("ok    %s (%s: %s)\n", f, p.Name, p.PortSummary())
					checkHelpers(p)
					continue
				}
			}
			bad++
			fmt.Printf("FAIL  %s\n", f)
			for _, line := range strings.Split(err.Error(), "\n") {
				fmt.Printf("        %s\n", line)
			}
		}
		if bad > 0 {
			return fmt.Errorf("%d file(s) have problems", bad)
		}
	case "new":
		return gameNew(store, args[1:])
	default:
		return fmt.Errorf("unknown game command %q", args[0])
	}
	return nil
}

func checkHelpers(p *profile.Profile) {
	if _, unknown := helpers.For(p); len(unknown) > 0 {
		fmt.Printf("      note: unknown helpers %v (ignored by this TailLAN version)\n", unknown)
	}
}

func gameNew(store *profile.Store, args []string) error {
	fs := flag.NewFlagSet("game new", flag.ExitOnError)
	name := fs.String("name", "", "game name")
	ports := fs.String("ports", "", "ports, e.g. \"7777/udp, 27015/tcp\"")
	connect := fs.String("connect", "", "what friends type in the game (default \"{{host}}:{{port}}\")")
	fromProc := fs.String("from-process", "", "take ports from a running program whose name contains this")
	fs.Parse(args)
	in := bufio.NewReader(os.Stdin)
	ask := func(q, def string) string {
		if def != "" {
			fmt.Printf("%s [%s]: ", q, def)
		} else {
			fmt.Printf("%s: ", q)
		}
		line, _ := in.ReadString('\n')
		if line = strings.TrimSpace(line); line == "" {
			return def
		}
		return line
	}
	if *fromProc != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		socks, err := portscan.Listening(ctx)
		cancel()
		if err != nil {
			return err
		}
		for _, pr := range portscan.ByProcess(socks) {
			if strings.Contains(strings.ToLower(pr.Name), strings.ToLower(*fromProc)) && !pr.Self {
				fmt.Printf("%s (PID %d) listens on %s\n", pr.Name, pr.PID, pr.Spec)
				*ports = pr.Spec
				if *name == "" {
					*name = strings.TrimSuffix(pr.Name, ".exe")
				}
				break
			}
		}
		if *ports == "" {
			return fmt.Errorf("no running program matching %q is listening; start the game's server first (see \"taillan detect\")", *fromProc)
		}
	}
	if *name == "" {
		*name = ask("Game name", "")
	}
	if *name == "" {
		return errors.New("a name is required")
	}
	if *ports == "" {
		fmt.Println("Ports look like: 25565 (TCP+UDP), 7777/udp, 2456-2457/udp, 27015/tcp. Tip: \"taillan detect\" shows what a running game uses.")
		*ports = ask("Ports", "")
	}
	pts, err := profile.ParsePorts(*ports)
	if err != nil {
		return err
	}
	p := &profile.Profile{ID: profile.Slug(*name), Name: *name, Ports: pts, Connect: *connect}
	if p.Connect == "" {
		p.Connect = ask("What friends type to connect ({{host}} = 127.0.0.1, {{port}} = first port)", "{{host}}:{{port}}")
	}
	p.Host.Steps = []string{"Start the game's server or host a lobby.", "Click Start sharing and send the invite code to your friends."}
	p.Join.Steps = []string{"In the game, connect to {{connect}}"}
	if store.Get(p.ID) != nil {
		if a := ask(fmt.Sprintf("A game with id %q exists; replace it with your version? (y/N)", p.ID), "n"); !strings.HasPrefix(strings.ToLower(a), "y") {
			return errors.New("cancelled")
		}
	}
	path, err := store.Save(p)
	if err != nil {
		return err
	}
	fmt.Printf("\nSaved %s\nEdit it to improve the host/join steps (see docs/adding-a-game.md), then share the file or open a pull request so everyone gets it.\n", path)
	return nil
}

// --- detect ---

func runDetect(args []string) error {
	fs := flag.NewFlagSet("detect", flag.ExitOnError)
	watch := fs.Bool("watch", false, "snapshot now, then show only ports opened after you start the game")
	all := fs.Bool("all", false, "include TailLAN's own ports")
	pos, _ := parseArgs(fs, args, false)
	filter := strings.Join(pos, " ")
	list := func() ([]portscan.Socket, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return portscan.Listening(ctx)
	}
	before, err := list()
	if err != nil {
		return err
	}
	socks := before
	if *watch {
		fmt.Println("Now start your game's server (or host a lobby in the game), then press Enter...")
		bufio.NewReader(os.Stdin).ReadString('\n')
		after, err := list()
		if err != nil {
			return err
		}
		old := map[string]bool{}
		for _, s := range before {
			old[fmt.Sprint(s.Proto, s.Port, s.PID)] = true
		}
		socks = nil
		for _, s := range after {
			if !old[fmt.Sprint(s.Proto, s.Port, s.PID)] {
				socks = append(socks, s)
			}
		}
		if len(socks) == 0 {
			fmt.Println("No new listening ports. Is the server fully started? Some games only open ports once a lobby is created.")
			return nil
		}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROGRAM\tPID\tPORTS")
	for _, pr := range portscan.ByProcess(socks) {
		if pr.Self && !*all {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(pr.Name), strings.ToLower(filter)) {
			continue
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", pr.Name, pr.PID, pr.Spec)
	}
	w.Flush()
	fmt.Println("\nShare one of these directly:  taillan host \"<ports>\" --name \"Game name\"")
	fmt.Println("Or save it as a game:         taillan game new --from-process <program>")
	return nil
}

// --- doctor ---

func runDoctor() error {
	fmt.Println("Checking your network (takes a few seconds)...")
	r := doctor.Run(context.Background())
	icons := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}
	for _, c := range r.Checks {
		fmt.Printf(" %s %-20s %s\n", icons[c.Status], c.Name, c.Detail)
	}
	fmt.Println("\n" + r.Summary)
	return nil
}
