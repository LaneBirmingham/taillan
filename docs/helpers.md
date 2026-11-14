# Writing a game helper

Most games need only a [game file](adding-a-game.md). A **helper** is a bit of Go code for the friction a JSON file can't remove, for example:

- the server's port changes every session, so detect it instead of asking the host to type it;
- the game finds servers only through LAN broadcasts, so re-announce the forwarded server on each friend's machine;
- the game wants a config file pointed at the server before it starts.

The built-in example is [`minecraft-lan`](../internal/helpers/minecraft.go). On the host it listens for Minecraft's *Open to LAN* multicast announcements and points the tunnel at whatever port Minecraft picked. On each friend's machine it announces the forwarded world, so it appears under *LAN worlds*.

## The interface

```go
// internal/helpers/helpers.go
type Helper interface {
	Name() string     // id that game files use: "helpers": ["my-game-lan"]
	Describe() string // one sentence for players, shown in the UI and `taillan helpers`
	Host(ctx context.Context, env *HostEnv) // runs on the host for the whole session
	Join(ctx context.Context, env *JoinEnv) // runs on each friend's machine while joined
}
```

Both methods run in their own goroutine and must return when `ctx` is cancelled (the session stopped). Either can be a no-op.

| `HostEnv` | |
|-----------|---|
| `Profile` | the game being shared, with any extra ports the host added |
| `HostName` | the host's display name |
| `SetTarget(group, port, note)` | forward port group `group` (an index into `Profile.Ports`) to `port` on the host's machine instead of the declared one; `note` is shown on the host screen |
| `Logf` | write to the activity log |

| `JoinEnv` | |
|-----------|---|
| `Profile` | the game as described by the invite |
| `HostName` | the host's display name |
| `LocalPort(group)` | the port group's first port as opened on this machine (it may differ from the game's default) |
| `Note(text)` | show a status line on the friend's screen (repeated notes are shown once) |
| `Logf` | write to the activity log |

## Steps

1. Create `internal/helpers/<game>.go`:

   ```go
   package helpers

   import "context"

   func init() { Register(myGameLAN{}) }

   type myGameLAN struct{}

   func (myGameLAN) Name() string     { return "my-game-lan" }
   func (myGameLAN) Describe() string { return "Shows the shared game in My Game's LAN server list." }

   func (myGameLAN) Host(ctx context.Context, env *HostEnv) {} // nothing to do on the host

   func (myGameLAN) Join(ctx context.Context, env *JoinEnv) {
   	// e.g. reply to the game's LAN discovery query with
   	// 127.0.0.1:env.LocalPort(0) until ctx is done.
   	<-ctx.Done()
   }
   ```

2. Enable it in the game file: `"helpers": ["my-game-lan"]`. If discovery makes the game connect to the machine's LAN address rather than `127.0.0.1`, also mark that port `"lan": true`.
3. Add tests next to it (see [`minecraft_test.go`](../internal/helpers/minecraft_test.go)): parse the game's packets, and run `Host`/`Join` against a fake env where possible.
4. `make test`, then try it for real: host in one TailLAN, join from a second copy (`TAILLAN_HOME=/tmp/friend taillan`).

## Rules

- **Stay on this machine.** Helpers run with the player's permissions. Only touch the local machine and the game's own traffic, never open new ports to the network, and never act on data from a friend beyond what the game itself would do.
- **Fail soft.** If something doesn't work (a socket is busy, the game isn't running), `Logf` it and return or keep waiting. Players can always fall back to typing the address, so never block or crash a session.
- **Be quiet.** Use `Note` once for something the player should know ("Listed under LAN worlds"), not for progress.
- **Don't rely on the host's code.** A friend may run an older TailLAN without your helper: unknown helper names are skipped with a log line, so the game must still work through the plain steps.
- **Mark your own traffic.** If you re-announce something, tag it (like Minecraft's `(via TailLAN)`) so another TailLAN on the same LAN doesn't pick it up as a real server.
- Use only the standard library and `golang.org/x/net` if you can; the binary has to cross-compile with `CGO_ENABLED=0` for Windows, macOS and Linux (`make release`).
