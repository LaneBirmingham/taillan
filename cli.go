package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// envPrefix starts the environment variables that set a command's
// flags: --keep is TAILLAN_KEEP, --display-name TAILLAN_DISPLAY_NAME.
// Handy where a command line is awkward to change, such as a container
// image's default command or a systemd unit.
const envPrefix = "TAILLAN_"

// envName is the environment variable for a flag.
func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// parseArgs parses flags that may come before or after the command's
// other arguments ("taillan host valheim --keep" and "taillan host
// --keep valheim" both work), and returns those other arguments. With
// env, flags not given on the command line are read from TAILLAN_*
// environment variables first.
func parseArgs(fs *flag.FlagSet, args []string, env bool) ([]string, error) {
	if env {
		if err := flagsFromEnv(fs); err != nil {
			return nil, err
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// After "--", everything is an argument.
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// serviceLink matches the variables Kubernetes (and Docker's legacy
// links) set for a service named "taillan": TAILLAN_PORT=tcp://10.0.0.5:80.
// They aren't settings, so they're ignored.
var serviceLink = regexp.MustCompile(`^(tcp|udp|sctp)://`)

// flagsFromEnv sets each flag from its TAILLAN_* variable, if set.
// Explicit flags parsed afterwards still win.
func flagsFromEnv(fs *flag.FlagSet) error {
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		v, ok := os.LookupEnv(envName(f.Name))
		if !ok || v == "" {
			return
		}
		if serviceLink.MatchString(v) {
			fmt.Fprintf(os.Stderr, "Note: ignoring %s=%s; it looks like a Kubernetes service address, not a setting.\n", envName(f.Name), v)
			return
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			v = boolWord(v)
		}
		if err := fs.Set(f.Name, v); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %v", envName(f.Name), v, err))
		}
	})
	return errors.Join(errs...)
}

// boolWord accepts the usual ways of saying yes and no in a config
// file, on top of Go's own (1, t, true...).
func boolWord(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "y", "on":
		return "true"
	case "no", "n", "off":
		return "false"
	}
	return v
}

// envArg returns a command's main argument: the first of pos, else the
// named environment variable.
func envArg(pos []string, env string) string {
	if len(pos) > 0 {
		return strings.Join(pos, " ")
	}
	return strings.TrimSpace(os.Getenv(env))
}

// commandUsage prints a command's usage line, a note about it, its
// flags and, for commands that read TAILLAN_* variables, how.
func commandUsage(fs *flag.FlagSet, synopsis, note string, env bool) func() {
	return func() {
		out := fs.Output()
		fmt.Fprintf(out, "Usage: taillan %s [flags]\n\n", synopsis)
		if note != "" {
			fmt.Fprintf(out, "%s\n\n", note)
		}
		fmt.Fprintln(out, "Flags:")
		fs.PrintDefaults()
		if env {
			fmt.Fprintln(out, "\nEvery flag can also be set with an environment variable: TAILLAN_ and the\nflag name in capitals, such as TAILLAN_KEEP=1 for --keep.")
		}
	}
}

// parseTargets reads --port: the host-side port of the first port
// group ("7777"), or of each group in order ("7777,8888"; leave one
// empty to keep that group's usual port).
func parseTargets(s string, groups int) (map[int]uint16, error) {
	out := map[int]uint16{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) > groups {
		return nil, fmt.Errorf("--port has %d ports, but this game has only %d port group(s)", len(parts), groups)
	}
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.ParseUint(part, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("--port %q: ports are numbers from 1 to 65535", part)
		}
		out[i] = uint16(n)
	}
	return out, nil
}

// interactive reports whether stdin is a terminal someone can type in,
// rather than a pipe, a file or nothing (a service or container; their
// /dev/null is a character device, but not a terminal).
func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
