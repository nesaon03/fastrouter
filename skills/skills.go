// Package skills embeds the Agent Skills (SKILL.md files) that the dashboard
// links to and the server serves at /skills/<name>/SKILL.md.
package skills

import "embed"

// FS holds every <name>/SKILL.md.
//
//go:embed */SKILL.md
var FS embed.FS
