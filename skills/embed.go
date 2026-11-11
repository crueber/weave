// Package skills embeds the generated Weave agent skill (skills/weave) and its compact
// instructions form (skills/instructions.md) so the CLI can print and install them without
// network access. Regenerate with `make skill`.
package skills

import "embed"

// FS contains weave/SKILL.md, weave/references/* and instructions.md.
//
//go:embed all:weave instructions.md
var FS embed.FS
