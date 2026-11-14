# Adding a game

A game in TailLAN is one small JSON file: which ports the game's server uses, and a few lines of instructions for the host and for friends. No code. Most games take five minutes.

You don't need a game file to play: **Host a game → Another game** shares any ports right away. A game file just makes it one click next time, and gives friends the right instructions.

## 1. Find the ports

Pick whichever is easiest:

- **Search.** "<game> dedicated server ports" usually finds them on the game's wiki. Look for the *game* port (sometimes also a *query* port). Ignore Steam ports (27036, 27015-27030 for Steam itself) unless it's a Source game.
- **Detect them.** In the app: *Tools → Port detector → Take snapshot*, start hosting in the game, then *Scan*. New ports are highlighted, and *Save as game* creates the file. In a terminal:

  ```sh
  taillan detect --watch          # snapshot, start the game, press Enter: shows only new ports
  taillan game new --from-process valheim_server   # turn a running program's ports into a game file
  ```

  Detection sees what a program *listens on*. Some games open their port only once you create a lobby or load a world, so do that before scanning.

## 2. Create the file

In the app: *Games → Add a game*, or *Save as game* on the host screen. In a terminal: `taillan game new` (asks for a name and ports). Either way the file lands in your games folder (*Games → Open games folder*, or `taillan game dir`), named `<id>.json`. You can also write it by hand:

```json
{
  "id": "space-haven",
  "name": "Space Haven",
  "aliases": ["spacehaven"],
  "ports": [
    { "port": 7777, "proto": "udp", "label": "Game" }
  ],
  "connect": "{{host}}:{{port}}",
  "host": {
    "steps": [
      "Start a multiplayer game from the main menu.",
      "Click Start sharing and send the invite code to your friends."
    ]
  },
  "join": {
    "steps": [
      "Multiplayer → Join by IP.",
      "Address: {{connect}}"
    ]
  },
  "notes": "Everyone needs the same game version.",
  "links": [{ "title": "Multiplayer guide", "url": "https://example.com/guide" }],
  "tags": ["survival"]
}
```

Optionally add a `"$schema"` line pointing at [`game.schema.json`](game.schema.json) (a file path or URL) to get autocomplete and checking in editors like VS Code. TailLAN ignores it.

Check it with:

```sh
taillan game validate ~/.config/taillan/games/space-haven.json
```

Files in your games folder are reloaded every time you open the Games list or host, so there's nothing to restart. A file with the same `id` as a built-in game replaces it for you (handy for fixing one); delete the file to go back.

## Fields

| Field | Required | What it does |
|-------|----------|--------------|
| `id` | yes | Lowercase letters, digits and dashes (`space-haven`). Stable: invites use it to match a friend's local copy. Name the file `<id>.json`. |
| `name` | yes | Shown everywhere. |
| `aliases` | | Extra search words (`"mc"`). |
| `ports` | yes | The ports the game's **server** listens on (below). |
| `connect` | | What friends type in the game. Default `{{host}}:{{port}}`. Use `{{host}}` alone for games that don't take a port, or a console command like `connect {{host}}:{{port}}`. |
| `host.steps` | recommended | What the host does before sharing. End with "Click Start sharing and send the invite code to your friends." |
| `join.steps` | recommended | What friends do in the game. Mention `{{connect}}`. |
| `helpers` | | Code helpers to enable, e.g. `["minecraft-lan"]` (see [helpers.md](helpers.md)). Unknown names are ignored. |
| `notes` | | Caveats: matching versions, crossplay off, mods. |
| `links` | | `{ "title", "url" }` to official server guides. |
| `tags` | | Free-form groups: `fps`, `survival`, `voice`... |

### Ports

Each entry is one port or an inclusive range:

| Field | What it does |
|-------|--------------|
| `port` | Port number (the first one, for a range). |
| `end` | Last port of a range: `{ "port": 2456, "end": 2457, "proto": "udp" }`. |
| `proto` | `tcp`, `udp`, or `both`. If you're unsure, `both` is safe. |
| `label` | What it is: `Game`, `Query`, `RCON`, `Voice`. |
| `samePort` | `true` if the game **can't** be told a port (only an IP), so friends must use exactly this port. If it's busy on their machine, joining fails with a clear message. |
| `ask` | Text for an input on the host screen, for servers whose port changes each session. The tunnel keeps the declared port, so invites stay the same. |
| `lan` | `true` to listen on all interfaces on friends' machines (still accepting connections only from that machine), for games that find servers by LAN broadcast and connect back to the LAN address. Causes a firewall prompt on Windows, so only use it when a game needs it. |

The short form used in the app and CLI maps onto these: `25565` (TCP+UDP), `7777/udp`, `2456-2457/u`, `27015/tcp`, comma-separated.

TailLAN forwards at most 64 individual ports per game, and port 65531 is reserved.

### Placeholders

Usable in `connect` and in steps. They're filled in on each friend's machine:

| Placeholder | Becomes |
|-------------|---------|
| `{{host}}` | `127.0.0.1` |
| `{{port}}` | the first port entry, as opened on this friend's machine |
| `{{port2}}`, `{{port3}}`... | later port entries |
| `{{connect}}` | the rendered `connect` value |
| `{{hostname}}` | the host's display name |

Ports can differ from the game's defaults on a friend's machine: if something already uses 7777 there, TailLAN picks another (moving ranges together) and the placeholders follow. That's why steps should say `{{connect}}` rather than a hard-coded `127.0.0.1:7777`.

## Writing good steps

- Short and literal: the menu path a player clicks. `Multiplayer → Join by IP`, not "navigate to the multiplayer section".
- Two to four steps each. Put caveats in `notes`.
- Host steps are about getting the game *listening*: start the dedicated server, open a lobby, open to LAN. If hosting from the game's own menu uses Steam invites instead of an IP, say so in `notes` and describe the dedicated-server route.
- Join steps end with where to type `{{connect}}`.

## Games that don't fit

TailLAN forwards ports from friends to the host. These need more than a game file:

- **The game can't connect by IP, only finds LAN servers by broadcast.** Mark the port `lan` and see if it shows up; if not, it needs a [helper](helpers.md) that re-announces the server on friends' machines (like Minecraft's).
- **The server's port changes every session.** Use `ask` so the host can type it; better, write a helper that detects it (`SetTarget`).
- **Steam/Epic-only matchmaking, consoles, or fully peer-to-peer games** aren't supported.

## Sharing your game

- **With friends:** you don't need to. Invite codes carry the game's ports, connect format and your custom steps, so friends see your instructions even if they've never heard of the game.
- **With everyone:** copy the file into [`games/`](../games) in this repository, add `"$schema": "../docs/game.schema.json"`, run `make test` (it validates every built-in game), and open a pull request. Please include where the port numbers came from.
