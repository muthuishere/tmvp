// Package protocol is the client↔server wire format: one JSON Request line,
// then a stream of JSONL Frames, ending with exactly one "done" frame.
package protocol

import (
	"os"
	"path/filepath"

	"github.com/muthuishere/tmvp/internal/errs"
)

// Version of the wire protocol; client and server must match.
const Version = 1

// Ops.
const (
	OpRun           = "run"   // read verb across selected targets (batch)
	OpWrite         = "write" // one env, one system, --reason required
	OpCheck         = "check"
	OpLogin         = "login"
	OpSecretsReload = "secrets.reload"
	OpSecretsClear  = "secrets.clear"
	OpSecretsStatus = "secrets.status"
	OpStatus        = "server.status"
	OpStop          = "server.stop"
	OpPing          = "ping"
)

// Request is sent by the client.
type Request struct {
	Proto int `json:"proto"`
	// Build is the client's SelfBuild; a server from another build answers
	// server_stale and the client restarts it (see client.Do).
	Build      string            `json:"build,omitempty"`
	Op         string            `json:"op"`
	ConfigPath string            `json:"config_path,omitempty"` // resolved by the client (lookup order)
	Product    string            `json:"product,omitempty"`     // informational; server trusts the config
	Env        string            `json:"env,omitempty"`         // env selector ("prod", "prod,staging", "all", "prod*")
	System     string            `json:"system,omitempty"`      // system name or glob ("" = all)
	Labels     string            `json:"labels,omitempty"`      // -l expression
	Verb       string            `json:"verb,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Flags      map[string]string `json:"flags,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Preview    bool              `json:"preview,omitempty"`
	Diff       bool              `json:"diff,omitempty"`
	Limit      int               `json:"limit,omitempty"`
	Since      string            `json:"since,omitempty"`
	Agent      string            `json:"agent,omitempty"` // caller agent (TMVP_AGENT / CLAUDECODE ...)
	Cwd        string            `json:"cwd,omitempty"`
	Timeout    string            `json:"timeout,omitempty"` // per-target timeout override
	// EnvSource says where Env came from: explicit | TMVP_ENV | use | default
	// ("" / none = nothing selected an env: reads run on all envs).
	EnvSource string `json:"env_source,omitempty"`
	// Force: on a write, allow a protected env; on a read, skip the cost
	// estimate (the timeout is still a hard kill).
	Force bool `json:"force,omitempty"`
}

// Env selection sources (Request.EnvSource, Frame.EnvSource).
const (
	EnvExplicit = "explicit" // positional selector or --env/-e
	EnvFromVar  = "TMVP_ENV"
	EnvFromUse  = "use"
	EnvDefault  = "default" // config default_env
	EnvNone     = "none"    // nothing selected: all envs
)

// Frame types.
const (
	FrameRow    = "row"
	FrameText   = "text"
	FrameError  = "error"  // a per-target error (batch continues)
	FrameStatus = "status" // per-target status: ok/failed/skipped (+ duration)
	FrameInfo   = "info"   // free-form structured info (server status, check tables)
	FrameDone   = "done"   // final; Exit is the process exit code
)

// Frame is one streamed line.
type Frame struct {
	Type    string         `json:"type"`
	Env     string         `json:"env,omitempty"`
	System  string         `json:"system,omitempty"`
	Row     map[string]any `json:"row,omitempty"`
	Text    string         `json:"text,omitempty"`
	Status  string         `json:"status,omitempty"` // ok | failed | skipped | truncated
	Error   *errs.Error    `json:"error,omitempty"`
	Elapsed float64        `json:"elapsed_ms,omitempty"`
	Exit    int            `json:"exit,omitempty"`
	Info    map[string]any `json:"info,omitempty"`
	// EnvSource is set on every frame of a run/write: how the env(s) were selected.
	EnvSource string `json:"env_source,omitempty"`
}

// Sink receives frames. Implementations must be safe for concurrent use.
type Sink func(Frame)

// Home is ~/.tmvp (TMVP_HOME overrides, used by tests).
func Home() string {
	if h := os.Getenv("TMVP_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tmvp")
}

// RunDir is ~/.tmvp/run (0700).
func RunDir() string { return filepath.Join(Home(), "run") }

// SocketPath is ~/.tmvp/run/tmvp.sock.
func SocketPath() string {
	if p := os.Getenv("TMVP_SOCKET"); p != "" {
		return p
	}
	return filepath.Join(RunDir(), "tmvp.sock")
}
