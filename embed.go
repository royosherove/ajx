// Package ajx embeds the skill's behavior-defining resources (prompts, schemas,
// references, page assets and examples) so the Go CLI is a single binary that
// shares them with the Python skill instead of copying them.
package ajx

import "embed"

// Version is the AJX release this CLI implements.
const Version = "1.1.0"

// Skill holds skills/ajx resources. Paths keep the "skills/ajx/" prefix; use
// internal/res for access that honors the AJX_SKILL_DIR override.
//
//go:embed skills/ajx/SKILL.md skills/ajx/prompts skills/ajx/schemas skills/ajx/references skills/ajx/assets skills/ajx/examples
var Skill embed.FS

// Installer is the reviewed installer shipped with this executable.
//
//go:embed install.sh
var Installer string
