// Package tmvp embeds the agent-facing documentation (llms.txt format,
// https://llmstxt.org): llms.txt (index), llms-full.txt (everything) and
// docs/llms/**. `tmvp llms` prints them.
//
// The per-type verb tables in docs/llms/types/*.md and llms-full.txt are
// generated from the connector registry; a test fails when they drift.
//
//go:generate sh -c "cd cmd/tmvp && TMVP_UPDATE_DOCS=1 go test -run TestLLMSDocsInSync ."
package tmvp

import "embed"

// Docs holds llms.txt, llms-full.txt and docs/llms/**.
//
//go:embed llms.txt llms-full.txt docs/llms
var Docs embed.FS
