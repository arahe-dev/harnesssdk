package harnesssdk

import _ "embed"

// Guide is the manual an agent reads before setting up or running a
// profile (harness guide).
//
//go:embed docs/GUIDE.md
var Guide string

// Skill is the Agent Skills file that tells a parent agent the harness
// command exists (harness skill). Copy it wherever the parent harness reads
// skills.
//
//go:embed skill/harness/SKILL.md
var Skill string

// Version is the HarnessSDK release.
const Version = "1.1.0"
