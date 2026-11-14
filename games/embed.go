// Package games embeds TailLAN's built-in game profiles.
//
// Each *.json file in this directory is one game. To add a game, copy
// an existing file, edit it, and run `taillan game validate
// games/<id>.json` (or `go test ./...`). See docs/adding-a-game.md.
package games

import "embed"

// FS holds the built-in profile files.
//
//go:embed *.json
var FS embed.FS
