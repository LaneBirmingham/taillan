# Contributing

Thanks for helping! The most useful contribution is a **game**: one JSON file, no Go needed.

## Add a game

1. Follow [docs/adding-a-game.md](docs/adding-a-game.md) and put the file in [`games/`](games) as `<id>.json`.
2. Run `make test` (it checks every built-in game) and try hosting it if you can.
3. Add the game to the table in [docs/README.md](docs/README.md#built-in-games), alphabetically.
4. Open a pull request. Say where the port numbers came from (a wiki page, the server's docs, `taillan detect`) and whether you tested it with a friend.

Not sure of the ports? Open a [game request](../../issues/new?template=game-request.yml) instead.

## Code changes

- Go version: see `mise.toml`. Build and test through the Makefile, which adds tailcat's build tags: `make build`, `make test`, `make vet`. Offline: `TAILLAN_SKIP_NET=1 make test`.
- Try host and join on one machine with two config folders: `./taillan` and `TAILLAN_HOME=/tmp/friend ./taillan`.
- Game-specific automation goes in a helper: [docs/helpers.md](docs/helpers.md).
- Keep player-facing text short and plain, and keep the security boundaries described in [AGENTS.md](AGENTS.md#code-conventions).
- Run `make fmt vet test` before opening a pull request.
