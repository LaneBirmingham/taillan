# Instructions for coding agents

TailLAN shares game servers between friends over [tailcat](https://github.com/tailscale/tailcat) tunnels. Read [README.md](README.md) for what it does, [docs/README.md](docs/README.md) for features, security and the code map, and [docs/server.md](docs/server.md) for headless and container use.

## Toolchain

- Go version is pinned in `mise.toml`; run commands through `mise exec -- go ...` if `go` isn't on PATH.
- Build and test with the tailcat build tags: `make build`, `make test`, `make vet`. The raw form is `go test -tags "$(cat build-tags.txt)" ./...`.
- `make test` includes a real host/join round trip over tailcat's public relays. Offline, use `TAILLAN_SKIP_NET=1 make test` (or `go test -short`).
- Everything must cross-compile with `CGO_ENABLED=0` (`make release`). Don't add cgo dependencies.
- The container image (`Dockerfile`) builds the same way; CI builds it and smoke-tests `host`, `status`, `invite` and `stop` inside it. Base images are pinned by digest (Dependabot bumps them).
- After adding or updating a dependency, run `make notices` and commit `THIRD_PARTY_NOTICES.txt` (CI checks it; release binaries embed it).

## Adding or fixing a game (most common task)

A game is one JSON file in [`games/`](games), embedded into the binary. Follow [docs/adding-a-game.md](docs/adding-a-game.md); the schema is [docs/game.schema.json](docs/game.schema.json). Checklist:

1. **Ports from a source, not memory.** Use the game's official docs or wiki for the dedicated-server ports and protocols, and cite the URL in `links`. If sources disagree or you can't verify the protocol, use `"proto": "both"`. Only the ports the *server listens on*; never Steam client ports or master-server ports.
2. **File name = id**: `games/<id>.json`, `id` matching `^[a-z0-9][a-z0-9-]{0,62}$`. First key `"$schema": "../docs/game.schema.json"`. Check no existing file covers the game (`grep -ril "<name>" games/`); extend `aliases` instead of duplicating.
3. **Connect format**: what the player literally types. Default `{{host}}:{{port}}`. `{{host}}` alone if the game takes no port, and then mark the port `"samePort": true`. Use console syntax where that's the way in (`connect {{host}}:{{port}}`).
4. **Steps**: `host.steps` gets the game listening (dedicated server, lobby, open to LAN) and ends with `Click Start sharing and send the invite code to your friends.` `join.steps` is the in-game menu path ending at `{{connect}}`. Two to four short steps each, using `→` between menu items. Never hard-code `127.0.0.1` or a port in steps; ports can be remapped on friends' machines.
5. **Notes** for caveats that break joining: version matching, crossplay must be off, platform-only invites.
6. **Don't add a game that can't work**: matchmaking-only, console-only, or full-mesh peer-to-peer games. If the game finds servers only by LAN broadcast, it needs a helper (below).
7. Validate: `go run -tags "$(cat build-tags.txt)" . game validate games/<id>.json`, then `make test` (`internal/profile` tests check that every built-in parses and has steps). Update the game table in docs/README.md, alphabetically.

## Helpers (game-specific code)

Only when JSON can't express it. See [docs/helpers.md](docs/helpers.md) and `internal/helpers/minecraft.go`. A helper must fail soft, touch only the local machine, return when its context ends, and come with tests.

## Code conventions

- Player-facing text (UI, CLI output, errors, game steps) is plain and short, written for someone who has never port-forwarded. Say what to do next, not what went wrong internally.
- The UI (`internal/ui/web`) is dependency-free HTML/CSS/JS embedded in the binary. Build DOM with the `h()` helper (it sets `textContent`); never use `innerHTML` with data. Keep it working in light and dark mode.
- Security boundaries to preserve: forwarded ports on joiners bind `127.0.0.1` (or all interfaces with the local-only filter for `lan` ports); the host only serves the profile's declared ports plus the control port (65531), and only to the chosen target; the UI and the terminal sessions' API (`internal/control`, used by `taillan status/kick/stop`) listen on `127.0.0.1` only, require the session cookie and check Host and Origin; `run/` records holding those secrets are `0600` in a `0700` folder. Add a test if you touch any of these.
- Headless use (servers, containers, `docs/server.md`) must keep working: never prompt when stdin isn't a terminal, keep every `host`/`join` flag settable through its `TAILLAN_*` variable, and keep `taillan status` exit codes (0 sharing or connected, 1 not).
- Invite format changes must stay backwards compatible within `TL1-` (add optional fields; never repurpose one).
- Run `gofmt`, `make vet` and `make test` before finishing.

## Commits

Don't commit unless asked. Commits carry only the configured git user: no AI co-author trailers or "generated with" footers.

Releases are cut by release-please from commit titles on `main`, so every commit title (and PR title, which becomes the squash commit) must be a [Conventional Commit](https://www.conventionalcommits.org/). PRs here are often merged with a merge commit, which keeps each branch commit, so each commit on the branch counts, not just the PR title. A title that doesn't parse is silently left out of the changelog.

- `feat:` new behaviour players or hosts notice (a new game is `feat(games): add Valheim`). Bumps the minor version.
- `fix:` a bug fix, including a corrected game profile (`fix(games): ...`). Bumps the patch version.
- `feat!:` or a `BREAKING CHANGE:` footer for incompatible changes (an invite older versions can't read, a removed flag or `TAILLAN_*` variable). Bumps the minor version until 1.0.
- `docs:`, `test:`, `refactor:`, `ci:`, `build:`, `chore:` don't release on their own.
- Optional scope is the area: `ui`, `games`, `helpers`, `session`, `invite`, `cli`, `server`, `docker`.
- Write the subject for the changelog: lower case, imperative, what changed for the user (`feat(cli): add taillan kick`), not how.
- One commit can carry several changelog entries: put extra `feat: ...` / `fix: ...` lines at the end of its body. A `Release-As: x.y.z` footer forces the next version.
- Never edit `CHANGELOG.md` or `.release-please-manifest.json` by hand; the release PR updates them.
