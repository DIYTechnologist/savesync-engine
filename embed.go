// Package savesyncengine embeds the built-in game metadata
// (games/*.json) so every consumer of this module (savesyncpspc,
// savecloud-agent, ...) shares one source of truth instead of each
// keeping its own copy. On-disk metadata passed to games.Profiles's
// gamesDir parameter still overrides/extends these defaults - Builtin
// only supplies what isn't found on disk.
package savesyncengine

import "embed"

//go:embed games/*.json
var Builtin embed.FS
