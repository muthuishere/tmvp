package secret

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/muthuishere/tmvp/internal/secure"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultTimeout bounds every secret / login-check command.
const DefaultTimeout = 30 * time.Second

// maxOutput caps a command's captured stdout.
const maxOutput = 4 << 20

// Runner runs a secret command and returns its output bytes.
// file=true means $file(cmd): the command writes to ${out} and the file
// content is returned instead of stdout.
type Runner interface {
	Run(ctx context.Context, cmd string, file bool, dir string) ([]byte, error)
}

// ExecRunner runs commands as real processes: never interactive (stdin is the
// null device), bounded by Timeout, argv split without a shell unless the
// command needs pipes/redirects.
type ExecRunner struct {
	Timeout time.Duration
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) {
	t := r.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	if !file {
		return Exec(ctx, cmd, dir)
	}
	tmp, err := secure.MkdirTemp("", "tmvp-out-") // owner-only on every OS
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "out")
	if _, err := Exec(ctx, strings.ReplaceAll(cmd, "${out}", quoteArg(out)), dir); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("$file command did not write ${out}: %w", err)
	}
	_ = os.Remove(out)
	return b, nil
}

func quoteArg(p string) string {
	if runtime.GOOS == "windows" || !strings.ContainsAny(p, " '\"") {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// Exec runs cmd non-interactively and returns stdout. The error message
// carries the exit status and the first stderr line (the caller redacts).
func Exec(ctx context.Context, cmd string, dir string) ([]byte, error) {
	var c *exec.Cmd
	if NeedsShell(cmd) {
		c = shellCommand(ctx, cmd)
	} else {
		argv, err := SplitArgs(cmd)
		if err != nil {
			return nil, err
		}
		if len(argv) == 0 {
			return nil, errors.New("empty command")
		}
		c = exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	c.Dir = dir
	c.Stdin = nil // null device: never interactive
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "TMVP_NONINTERACTIVE=1")
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxOutput, 4096
	c.Stdout, c.Stderr = &stdout, &stderr
	c.WaitDelay = 2 * time.Second
	setProcAttrs(c)
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("timed out: %w", ctx.Err())
		}
		line := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if line != "" {
			return nil, fmt.Errorf("%v: %s", err, line)
		}
		return nil, err
	}
	if stdout.over {
		return nil, fmt.Errorf("output larger than %d bytes", maxOutput)
	}
	return stdout.Bytes(), nil
}

func shellCommand(ctx context.Context, cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("pwsh"); err == nil {
			return exec.CommandContext(ctx, p, "-NoProfile", "-NonInteractive", "-Command", cmd)
		}
		return exec.CommandContext(ctx, "cmd", "/c", cmd)
	}
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}

type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// NeedsShell reports whether cmd uses shell syntax (pipes, redirects,
// sequencing, substitution, globs, env assignment) outside quotes.
func NeedsShell(cmd string) bool {
	q := byte(0)
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if q != 0 {
			if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			q = c
		case '|', '>', '<', ';', '&', '`', '*', '?':
			return true
		case '$':
			if i+1 < len(cmd) && (cmd[i+1] == '(' || cmd[i+1] == '{' || isNameChar(cmd[i+1])) {
				return true
			}
		}
	}
	return false
}

func isNameChar(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// SplitArgs splits a command line into argv: whitespace separated, with
// '…' (literal) and "…" (backslash escapes \" and \\) quoting. On Windows a
// backslash outside double quotes is literal (paths).
func SplitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	win := runtime.GOOS == "windows"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated ' in command")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unterminated \" in command")
			}
		case c == '\\' && !win && i+1 < len(s):
			inWord = true
			i++
			cur.WriteByte(s[i])
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// RunnerFunc adapts a func to Runner (tests, fakes).
type RunnerFunc func(ctx context.Context, cmd string, file bool, dir string) ([]byte, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) {
	return f(ctx, cmd, file, dir)
}
