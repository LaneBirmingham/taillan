# Running TailLAN on a server

TailLAN runs fully from the command line: no window, no browser, nothing to click. Use it to share a dedicated game server on a home server, a VPS, a NAS or a Raspberry Pi, over SSH, as a service, in Docker or on Kubernetes. Friends still join with an invite code, and nothing needs a port opened or forwarded.

- [Quick start over SSH](#quick-start-over-ssh)
- [Managing a running game](#managing-a-running-game)
- [Settings and environment variables](#settings-and-environment-variables)
- [Choosing a setup](#choosing-a-setup)
- [Docker](#docker)
- [Kubernetes](#kubernetes)
- [systemd](#systemd)
- [Several games on one server](#several-games-on-one-server)
- [The app's window on a headless server](#the-apps-window-on-a-headless-server)
- [Scripts and automation](#scripts-and-automation)
- [Security on a server](#security-on-a-server)
- [Troubleshooting](#troubleshooting)

```
  Server (VPS, NAS, container...)                      Friend's PC
 ┌───────────────────────────────────┐               ┌──────────────────┐
 │ game server :2456/udp             │               │ game client      │
 │        ▲                          │   encrypted   │ connects to      │
 │        │ 127.0.0.1, a LAN IP,     │    tunnel     │ 127.0.0.1:2456   │
 │        │ or a container name      │               │        │         │
 │ taillan host valheim --keep ◀─────┼───────────────┼── TailLAN (join) │
 │   ▲                               │  outbound     └──────────────────┘
 │   └─ taillan status / kick / stop │  only
 └───────────────────────────────────┘
```

## Quick start over SSH

```sh
ssh my-server
# On an ARM server (64-bit Raspberry Pi OS, ARM VPS), use taillan-linux-arm64.
curl -fL --create-dirs -o ~/.local/bin/taillan \
  https://github.com/LaneBirmingham/taillan/releases/latest/download/taillan-linux-amd64
chmod +x ~/.local/bin/taillan
export PATH="$HOME/.local/bin:$PATH"   # if "taillan" isn't found; add it to ~/.profile to keep it
taillan name "Lane's server"           # the name friends see
taillan host valheim --keep
```

There are builds for 64-bit Linux on x86 (amd64) and ARM (arm64). 32-bit systems aren't supported.

Start the game's dedicated server first (or after: TailLAN shows whether it's running). `taillan host` prints the invite code; send it to your friends privately. `--keep` gives you the same code every time, so a regular group only needs it once.

Closing the SSH session stops sharing, and friends are told the game ended. To keep it running after you log out, run it in `tmux` or `screen`, or as a [service](#systemd). `nohup taillan host valheim --keep &` also works.

TailLAN needs no root, no ports opened and no firewall changes: it only makes outbound connections. Run `taillan doctor` to check the server's network.

## Managing a running game

From any terminal on the same machine, as the same user (another SSH session, `docker exec`, `kubectl exec`):

| Command | What it does |
|---------|--------------|
| `taillan status` | The game, whether its server is running, the room, and who's connected (direct or relayed, ping). Exits 0 while sharing. |
| `taillan status --json` | The same as JSON, for scripts. It includes the invite code. |
| `taillan invite` | Print the invite code. `--message` prints a ready-to-send message for friends. |
| `taillan lock` | Stop new friends joining with the invite. Friends already here can reconnect. |
| `taillan unlock` | Let anyone with the invite join again. |
| `taillan kick <name>` | Remove a friend. This also locks the room, since anyone with the invite could come back under a new identity. |
| `taillan stop` | Stop sharing (or leave a game you joined). `--all` stops everything. |
| `taillan name [new name]` | Show or change the name friends see. |
| `taillan retire <game>` | Retire a kept invite code: next time you share that game, it gets a new one. |

These work the same whether the game was started with `taillan host`, from the app's window, as a service or in a container. If several games are running, say which with `--game <id>` (or the process ID): `taillan kick sam --game valheim`.

In a container, `taillan stop` ends the container's main process, so a restart policy (`restart: unless-stopped`, or any Kubernetes Deployment) starts sharing again straight away, with the same kept code. To stop sharing for good, stop the container instead: `docker compose stop taillan`, `docker stop`, or `kubectl scale deploy/<name> --replicas=0`. In the [sidecar](#sidecar-sharing-the-network) setup, stop the game server too.

## Settings and environment variables

Everything `taillan host` needs fits on its command line:

```sh
taillan host valheim                              # a built-in game (taillan games lists them)
taillan host "7777/udp, 27015/tcp" --name "Space Game"   # any game, by its ports
taillan host minecraft-java --target 192.168.1.20 # the server runs on another machine
taillan host minecraft-java --port 25566          # the server listens on a different port
taillan host satisfactory --port 15777,8889       # ... per port group, in order
taillan host valheim --extra 64738                # share a voice server too
```

Every flag of `host`, `join` and `ui` can also be set with an environment variable: `TAILLAN_` and the flag name in capitals, with dashes as underscores. Flags on the command line win. That's handy in container images, Compose files and systemd units:

| Variable | Flag | Meaning |
|----------|------|---------|
| `TAILLAN_GAME` | *(argument)* | The game id, or ports like `7777/udp`. |
| `TAILLAN_TARGET` | `--target` | Where the game server runs: `127.0.0.1` (default), another device's IP, or a host name such as a container's service name (looked up each time a friend connects). |
| `TAILLAN_PORT` | `--port` | Where the server listens, if not on the game's usual port. One port, or one per port group: `7777,8888`. |
| `TAILLAN_KEEP` | `--keep` | `1`/`true`/`yes`: the same invite code every time. The key is saved in `$TAILLAN_HOME/keys`. |
| `TAILLAN_DISPLAY_NAME` | `--display-name` | The name friends see, for this run. `taillan name` saves one for good. |
| `TAILLAN_INVITE_FILE` | `--invite-file` | Also write the invite code to this file (only you can read it). Removed when sharing stops. |
| `TAILLAN_PRINT_INVITE` | `--print-invite` | `false` keeps the invite code out of the output, and so out of logs. |
| `TAILLAN_NAME` | `--name` | The game's name when sharing by ports. |
| `TAILLAN_EXTRA` | `--extra` | Extra ports to share along with the game. |
| `TAILLAN_INVITE` | *(argument)* | For `taillan join`: the invite code. |
| `TAILLAN_ALLOW_LAN` | `--allow-lan` | For `taillan join`: let other LAN devices use ports the game marks for LAN play. |
| `TAILLAN_HOME` | | Where TailLAN keeps its state (below). |

TailLAN keeps a little state in its config folder (`~/.config/taillan` on Linux, or `$TAILLAN_HOME`):

| | |
|-|-|
| `settings.json` | Your name, and this machine's identity for joining. |
| `keys/` | Kept invite keys (`--keep`). Secret: anyone with one can pose as your game. |
| `games/` | Your own game files (`taillan game dir`). Add games there to share them by name. |
| `run/` | How `taillan status` and friends reach running sessions. Removed on exit. |

If a game you want isn't built in, either share it by ports or drop a [game file](adding-a-game.md) into `games/`.

## Choosing a setup

| Setup | Use it when | `--target` | Server check |
|-------|-------------|------------|--------------|
| [Same machine](#quick-start-over-ssh) | The game server runs on the box you SSH into. | default | ✓ |
| [Another LAN device](#another-device-on-the-lan) | A Pi or NAS shares a server on a gaming PC. | that device's IP | – |
| [Docker, same network](#same-network---target) | The simplest container setup. Either side can restart. | the service name | – |
| [Docker sidecar](#sidecar-sharing-the-network) | You want the server check, or the server binds 127.0.0.1. | default | ✓ |
| [Docker host networking](#host-networking) | Servers already run on the host or publish their ports. | default | ✓ |
| [Kubernetes](#kubernetes) | A pod runs the server; TailLAN goes next to it. | default | ✓ |
| [systemd](#systemd) | A plain server, started at boot. | either | ✓ |

*Server check* means `taillan status` can tell whether the game server is listening. It looks at the local machine's sockets, so it can't see servers on other machines or in other containers' networks; sharing still works there.

### Another device on the LAN

Any always-on machine can share a server running elsewhere on the same network:

```sh
taillan host valheim --target 192.168.1.20 --keep
```

Friends reach only the game's ports on that one address. Give the game server a fixed LAN IP (a DHCP reservation in your router), or use its host name if your network resolves it (`--target gaming-pc.lan`).

## Docker

The image is `ghcr.io/lanebirmingham/taillan` (tags: `latest` and each release, `linux/amd64` and `linux/arm64`). It's built from the [`Dockerfile`](../Dockerfile) in this repository: `docker build -t taillan .` builds it yourself.

- Its entrypoint is `taillan` and its default command `host`: pass a command and its flags (`docker run ... ghcr.io/lanebirmingham/taillan host valheim --keep`), or set `TAILLAN_GAME=valheim` (and other `TAILLAN_*` variables) and pass nothing.
- State lives in `/data`. Mount a volume there, or the invite code changes every time the container is recreated.
- It runs as a non-root user (65532) with no shell. A bind-mounted `/data` must be writable by that user: `sudo chown 65532:65532 ./taillan-data`, or set `user:` to your own IDs.
- It has a health check (`taillan status --quiet`): healthy while sharing.
- Manage it with `docker exec <container> taillan status` (or `invite`, `kick`, ...).
- `docker stop` stops sharing cleanly and tells friends.

### Same network (`--target`)

TailLAN runs as its own service and forwards friends to the game server by its service name. Nothing is shared between the containers, so either can restart or update without affecting the other.

```yaml
services:
  minecraft:
    image: itzg/minecraft-server
    environment:
      EULA: "TRUE"
    volumes: [minecraft:/data]
    restart: unless-stopped

  taillan:
    image: ghcr.io/lanebirmingham/taillan:latest
    command: ["host", "minecraft-java", "--target", "minecraft", "--keep"]
    environment:
      TAILLAN_DISPLAY_NAME: "Lane's server"
    volumes: [taillan:/data]
    restart: unless-stopped

volumes:
  minecraft:
  taillan:
```

Full file: [`examples/compose-network.yml`](../examples/compose-network.yml). Get the code with `docker compose exec taillan taillan invite`. The game server needs no `ports:` for friends; keep them only if LAN players connect directly.

### Sidecar sharing the network

The game server runs inside TailLAN's network namespace (`network_mode: "service:taillan"`), so TailLAN reaches it on `127.0.0.1`, sees whether it's listening, and works with servers that only listen on localhost.

```yaml
services:
  taillan:
    image: ghcr.io/lanebirmingham/taillan:latest
    command: ["host", "valheim", "--keep"]
    volumes: [taillan:/data]
    restart: unless-stopped

  valheim:
    image: ghcr.io/community-valheim-tools/valheim-server
    network_mode: "service:taillan"
    depends_on: [taillan]
    environment:
      SERVER_NAME: "Game night"
      SERVER_PASS: "change-me"
      SERVER_PUBLIC: "false"
    volumes: [valheim-config:/config, valheim-data:/opt/valheim]
    restart: unless-stopped
```

Full file: [`examples/compose-sidecar.yml`](../examples/compose-sidecar.yml).

Why the game joins TailLAN's network and not the other way round: a container that borrows another's network loses it when that one restarts. Game servers restart for updates and crashes far more often than TailLAN does, so the game is the one that borrows. If TailLAN's container restarts (after `taillan stop`, a crash, or a reboot), the game server is left without a network until it restarts too: `docker compose restart valheim`. If TailLAN's container is recreated, `docker compose up -d` recreates the game server too. To let LAN players connect directly, publish the game's ports on the `taillan` service, since that's where the network lives.

### Host networking

With `network_mode: host` (Linux only), TailLAN sees the host's network as if it ran there: game servers on the host, or containers that publish their ports, are on `127.0.0.1`.

```yaml
services:
  taillan:
    image: ghcr.io/lanebirmingham/taillan:latest
    network_mode: host
    command: ["host", "terraria", "--keep"]
    volumes: [taillan:/data]
    restart: unless-stopped
volumes:
  taillan:
```

### `docker run`

```sh
docker run -d --name taillan --restart unless-stopped \
  -v taillan:/data -e TAILLAN_DISPLAY_NAME="Lane's server" \
  --add-host host.docker.internal:host-gateway \
  ghcr.io/lanebirmingham/taillan host factorio --target host.docker.internal --keep
docker exec taillan taillan invite
```

This shares a Factorio server running on the Docker host itself (it must listen on all interfaces, which servers usually do). For a server in another container, use `--network container:<name>` instead of `--add-host`, and drop `--target`.

## Kubernetes

Containers in a pod share one network, so TailLAN is a natural sidecar: it reaches the game server on `127.0.0.1`, and either container can restart on its own. No Service, LoadBalancer or Ingress is needed for friends.

```yaml
      containers:
        - name: minecraft
          image: itzg/minecraft-server
          env: [{ name: EULA, value: "TRUE" }]
        - name: taillan
          image: ghcr.io/lanebirmingham/taillan:latest
          args: ["host", "minecraft-java", "--keep", "--print-invite=false"]
          volumeMounts: [{ name: taillan, mountPath: /data }]
```

Full manifest with volumes, security context and a readiness probe: [`examples/kubernetes.yml`](../examples/kubernetes.yml). Then:

```sh
kubectl exec deploy/minecraft -c taillan -- taillan invite
kubectl exec deploy/minecraft -c taillan -- taillan status
```

Run one replica with the `Recreate` strategy: two pods with the same kept invite would fight over one identity. Keep `/data` on a persistent volume, or the code changes with every new pod. The example sets `enableServiceLinks: false`: if a Service is named `taillan`, Kubernetes otherwise adds variables like `TAILLAN_PORT=tcp://...` to the pod (TailLAN ignores those, but it's tidier without them).

## systemd

A user service needs no root, starts at boot, and restarts if it stops unexpectedly. [`examples/systemd/taillan@.service`](../examples/systemd/taillan@.service) runs one instance per game:

```sh
mkdir -p ~/.config/systemd/user
cp taillan@.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now taillan@valheim
loginctl enable-linger "$USER"        # keep it running when you're logged out

taillan status
journalctl --user -u taillan@valheim -f
```

Per-game settings go in `~/.config/taillan/<game>.env` (`TAILLAN_TARGET=...`, `TAILLAN_PORT=...`), which the unit reads.

To run it as a system service for a dedicated account instead, use a unit like this, and run `taillan` commands as that account (`sudo -u games taillan status`):

```ini
[Service]
User=games
ExecStart=/usr/local/bin/taillan host valheim --keep
Restart=on-failure
```

TailLAN waits for the network by itself (it retries until the relays answer), so the unit needs no `network-online.target` ordering.

## Several games on one server

Run one `taillan host` per game, each its own process or service. On one machine they share one config folder: each game keeps its own invite key, and the commands above ask which game you mean when there's more than one:

```sh
systemctl --user enable --now taillan@valheim taillan@minecraft-java
taillan status
taillan invite --game minecraft-java
taillan stop --all
```

In containers, give each TailLAN container its own `/data` volume, and manage each one with `docker exec` into that container: commands only see sessions in their own container.

Two servers of the same game work too (`taillan host valheim` and `taillan host valheim --port 2466`); pick one by its process ID (`--game <PID>`). Only one of them can use the game's kept invite (`--keep`), since a kept invite is one identity. For two kept codes, share the second by its ports with its own `--name` (`taillan host 2466-2467/udp --name "Valheim 2" --keep`), or give it its own `TAILLAN_HOME`.

## The app's window on a headless server

If you'd rather click than type, run the app with no window and reach it through SSH:

```sh
ssh -L 47800:127.0.0.1:47800 my-server
taillan ui --no-open --stay
```

Open the `http://127.0.0.1:47800/?t=...` link it prints in your own browser. You get the whole app (pick a game, see friends, lock, remove) for a server with no screen. `--stay` keeps it running after you close the browser. The port in the link must match on both ends of the tunnel; if 47800 was busy, use the one it printed.

The app only ever listens on `127.0.0.1` and needs the secret in that link, so an SSH tunnel is the way in. Don't expose it with a proxy.

## Scripts and automation

- **Invite in a file:** `--invite-file "$XDG_RUNTIME_DIR/taillan/invite.txt"` writes the code once it's ready and deletes it when sharing stops. Wait for the file to appear to know sharing has started.
- **Status as JSON:** `taillan status --json` lists every running session: game, invite, room lock, port groups and friends with their connection. A session still starting has `"starting": true` and `"session": null`.

  ```sh
  taillan status --json | jq -r '.[] | .session.friends[]? | select(.online) | .name'
  ```

- **Exit codes:** `taillan status` exits 0 while a game is shared (or joined and connected) and 1 otherwise; `--quiet` prints nothing. Good for health checks and monitoring.
- **No prompts:** with no terminal attached (a service, a container, a pipe), TailLAN never waits for input; a missing invite or game is an error instead.
- **Signals:** SIGTERM, SIGINT and SIGHUP stop sharing cleanly and tell friends. `taillan host` retries by itself while the network is down at start, so it's safe to start at boot.

## Security on a server

- **The invite code is the key.** Anyone with it can connect while you share. `taillan host` prints it, so it ends up in `journalctl`, `docker logs` and log collectors. If other people can read your logs, use `--print-invite=false` and get the code with `taillan invite` or `--invite-file` (readable only by TailLAN's user).
- **Kept invites** (`--keep`) are saved in `keys/` in the config folder. Treat that volume or folder, and its backups, as secret. `taillan retire <game>` retires one.
- **Only the game's ports are reachable**, and only on the address you chose with `--target`. Nothing else on the server or its network is exposed. The game's ports aren't opened on the server's network interfaces either: the only thing TailLAN listens on there is the tunnel's own UDP socket, which only accepts encrypted traffic from invite holders.
- **Managing sessions is local only.** Every running TailLAN serves its API on `127.0.0.1` behind a random secret kept in `run/`, which only TailLAN's user (and root) can read. In a sidecar setup the game server shares TailLAN's loopback, but not its `/data` volume, so it can't use the API.
- **Lock the room** once your regular group has joined; `kick` locks it for you.
- **Joining from a server** (`taillan join`) opens the game's ports on that server's `127.0.0.1` only, for programs on the same machine (a bot, a test). Ports a game marks for LAN play listen on all interfaces but still refuse other devices unless you pass `--allow-lan`.

## Troubleshooting

| Problem | What to do |
|---------|------------|
| `Couldn't reach a relay server` | The server has no internet yet, or blocks outbound traffic. TailLAN keeps retrying. It needs outbound HTTPS (TCP 443) to reach the relays, and outbound UDP for fast direct connections. `taillan doctor` checks both. |
| Status says `server NOT running` | The game server isn't listening on that port yet, or listens on another one: start it, or pass `--port`. In a container, check it's in the same network namespace (sidecar) or use `--target`. |
| Status says `can't tell if the server is running` | Normal with `--target` pointing elsewhere. If friends can't connect, the log says what happened (`nothing answered on port ...`). |
| The invite code changed after a restart | Use `--keep`, and keep `TAILLAN_HOME` (`/data` in containers) on a persistent volume. The code also changes if the relay it used is retired; the log says so. |
| `permission denied` under `/data` | A bind mount not writable by user 65532: `chown 65532:65532` it, or run the container as your own user. |
| `TailLAN isn't running` from `taillan status` | Run it as the same user, with the same `TAILLAN_HOME`, as the session (`docker exec`, `sudo -u`). |
| Friends connect but are always *Relayed* | Both sides are behind strict NATs, or a firewall blocks UDP. It works, with more lag. `taillan doctor` shows the NAT type and whether UDP works. |
