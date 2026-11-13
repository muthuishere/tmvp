package tmvp

import "embed"

// Skills holds the agent skills shipped with tmvp (skills/<name>/**);
// `tmvp skill install` copies them into the user's skill folders.
//
//go:embed skills
var Skills embed.FS
