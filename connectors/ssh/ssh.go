// Package ssh is tmvp's ssh connector and ssh tunneler (`via:`).
//
// Config (system fields):
//
//	host, port (22), user, key (PEM private key; "${file:secrets.x}" or a secret string),
//	passphrase, password (optional), host_key ("ssh-ed25519 AAAA..." authorized_keys line(s))
//	or known_hosts (known_hosts content), insecure: true (skip host key check — explicit only).
//	read.* / write.* override user/key/password per mode.
//
// The key never touches disk: it is parsed from the secret bytes in memory.
//
// Verbs: ls (saved scripts + running containers), health, docker (read allow-list:
// ps/logs/inspect/stats/top/images/compose ps|logs; inspect masks Config.Env), script @name (read; the saved script under assets/<system>/scripts/<name>.sh
// must declare `read: true` or `mode: read` in its front-matter), run "<cmd>"|@name (write, the
// default write verb). script/run take -F container=<c> to run inside a container
// (docker exec over ssh). Commands run under `nice -n 10`, with the per-call timeout and line/byte caps.
package ssh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // known_hosts hashing is HMAC-SHA1 by definition
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() {
	connector.Register("ssh", New)
	connector.RegisterTunneler("ssh", Tunneler{})
	connector.RegisterTunneler(config.ContainerHopType, ContainerTunneler{})
}

// New returns an unopened ssh connector.
func New() connector.Connector { return &SSH{} }

// SSH is one ssh client for one env/system/mode.
type SSH struct {
	t      *connector.Target
	client *ssh.Client
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — saved scripts across all asset roots: @name, read|write, description, source; plus running containers (name, image, status, ports)"},
	{Name: "health", Mode: connector.Read, Help: "connect, uname, uptime, latency"},
	{Name: "docker", Mode: connector.Read, Help: "docker ps [-a] | logs <c> [--tail N] [--since X] [--timestamps] | inspect <c> (env masked) | stats [c] | top <c> | images | compose ps | compose logs <svc> (-F project=<dir> | -F file=<path>) — allow-list only"},
	{Name: "script", Mode: connector.Read, Saved: true, Help: "script @name [args...] [-F container=<c>] — saved script declared read (assets/<system>/scripts/<name>.sh); in a container via docker exec -i"},
	{Name: "run", Mode: connector.Write, Default: true, Help: `run "<command>"|@name [args...] [-F container=<c>] (tmvp write; nice + timeout + output caps; in a container via docker exec)`},
}

func (s *SSH) Type() string            { return "ssh" }
func (s *SSH) Verbs() []connector.Verb { return verbs }

// Guard rejects bad arguments before any connection is made.
func (s *SSH) Guard(c connector.Call) error {
	switch c.Verb {
	case "script":
		if len(c.Args) == 0 || !strings.HasPrefix(c.Args[0], "@") {
			return errs.New(errs.PolicyBlocked, "read scripts must be saved scripts (@name)").
				WithHint(`ad-hoc commands are writes: tmvp write <env> <system> "<cmd>" --reason "why"`)
		}
	case "run":
		if len(c.Args) == 0 || strings.TrimSpace(c.Args[0]) == "" {
			return errs.New(errs.Usage, "missing command").WithHint(`run "<command>" or run @saved`)
		}
	case "docker":
		if _, err := dockerArgv(c.Args, c.Flags, c.Since, c.Limit); err != nil {
			return err
		}
	}
	if ct := c.Flags["container"]; ct != "" && (c.Verb == "run" || c.Verb == "script") {
		return validName(ct)
	}
	return nil
}

// Open connects (through t.Dialer, so via: chains apply) and authenticates.
func (s *SSH) Open(ctx context.Context, t *connector.Target) error {
	s.t = t
	c, err := Dial(ctx, t, t.Dialer)
	if err != nil {
		return err
	}
	s.client = c
	return nil
}

// Close closes the client.
func (s *SSH) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// Check implements connector.Checker.
func (s *SSH) Check(ctx context.Context) error {
	_, _, err := s.client.SendRequest("keepalive@openssh.com", true, nil)
	if err != nil {
		return errs.Wrap(errs.Unreachable, err, "ssh keepalive")
	}
	return nil
}

// Do runs one verb.
func (s *SSH) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := s.Guard(c); err != nil {
		return err
	}
	switch c.Verb {
	case "ls":
		items, err := s.t.ListSaved("scripts", ".sh")
		if err != nil {
			return errs.Wrap(errs.Runtime, err, "")
		}
		for _, sv := range items {
			mode := "write"
			if DeclaredRead(sv.FrontMatter) {
				mode = "read"
			}
			if err := e.Row(connector.Row{"kind": "scripts", "name": "@" + sv.Name, "mode": mode,
				"description": sv.Description(), "source": sv.Path}); err != nil {
				return ignoreLimit(err)
			}
		}
		return s.lsContainers(ctx, e)
	case "docker":
		return s.docker(ctx, c, e)
	case "health":
		start := time.Now()
		var out []string
		err := s.exec(ctx, "uname -a; uptime", connector.EmitFuncs{TextFn: func(l string) error { out = append(out, l); return nil }}, 20, connector.CallTimeout(c, s.t.Limits))
		if err != nil {
			return err
		}
		r := connector.Row{"ok": true, "latency_ms": time.Since(start).Milliseconds(),
			"server_version": string(s.client.ServerVersion())}
		if len(out) > 0 {
			r["uname"] = out[0]
		}
		if len(out) > 1 {
			r["uptime"] = strings.TrimSpace(out[1])
		}
		return ignoreLimit(e.Row(r))
	case "script":
		sv, err := s.t.LoadSaved("scripts", c.Args[0], ".sh")
		if err != nil {
			return errs.Wrap(errs.NotFound, err, "")
		}
		if !DeclaredRead(sv.FrontMatter) {
			return errs.New(errs.PolicyBlocked, "script %s is not declared read (front-matter `read: true`)", sv.Name).
				WithHint(`run it as a write: tmvp write <env> <system> @` + sv.Name + ` --reason "why"`)
		}
		cmd := withArgs(sv.Body, c.Args[1:])
		if ct := c.Flags["container"]; ct != "" {
			if ct, err = s.resolveContainer(ctx, ct, c.Flags["sudo"] == "true"); err != nil {
				return err
			}
			cmd = scriptInContainer(ct, sv.Body, c.Args[1:], c.Flags["sudo"] == "true")
		}
		return s.exec(ctx, cmd, e, c.Limit, connector.CallTimeout(c, s.t.Limits))
	case "run":
		if s.t.Mode != connector.ModeWrite {
			return errs.New(errs.PolicyBlocked, "run needs tmvp write").WithHint(`tmvp write <env> <system> "<cmd>" --reason "why"`)
		}
		cmd := strings.Join(c.Args, " ")
		if ct := c.Flags["container"]; ct != "" {
			n, err := s.resolveContainer(ctx, ct, c.Flags["sudo"] == "true")
			if err != nil {
				return err
			}
			c.Flags["container"] = n
		}
		if strings.HasPrefix(c.Args[0], "@") {
			sv, err := s.t.LoadSaved("scripts", c.Args[0], ".sh")
			if err != nil {
				return errs.Wrap(errs.NotFound, err, "")
			}
			cmd = withArgs(sv.Body, c.Args[1:])
			if ct := c.Flags["container"]; ct != "" {
				cmd = scriptInContainer(ct, sv.Body, c.Args[1:], c.Flags["sudo"] == "true")
			}
		} else if ct := c.Flags["container"]; ct != "" {
			cmd = inContainer(ct, cmd, c.Flags["sudo"] == "true")
		}
		if c.Preview {
			return ignoreLimit(e.Row(connector.Row{"preview": true, "command": firstLine(cmd), "note": "not executed"}))
		}
		return s.exec(ctx, cmd, e, c.Limit, connector.CallTimeout(c, s.t.Limits))
	}
	return errs.New(errs.Usage, "ssh has no verb %q", c.Verb)
}

// DeclaredRead reports whether a script's front-matter declares it read-only.
func DeclaredRead(fm string) bool {
	for _, l := range strings.Split(fm, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if (k == "read" && v == "true") || (k == "mode" && v == "read") {
			return true
		}
	}
	return false
}

// withArgs passes args as $1.. to a script body via `sh -s`.
func withArgs(body string, args []string) string {
	if len(args) == 0 {
		return body
	}
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shQuote(a)
	}
	return "set -- " + strings.Join(q, " ") + "\n" + body
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// exec runs cmd under nice with the call's timeout, streaming stdout+stderr lines.
func (s *SSH) exec(ctx context.Context, cmd string, e connector.Emit, limit int, timeout time.Duration) error {
	lim := s.t.Limits
	lim.Timeout = timeout
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()
	sess, err := s.client.NewSession()
	if err != nil {
		return errs.Wrap(errs.Unreachable, err, "ssh session")
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(cmd)
	pr, pw := io.Pipe()
	sess.Stdout, sess.Stderr = pw, pw
	secs := int(lim.Timeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	// server-side guard too: `timeout` when present, always nice'd
	remote := fmt.Sprintf("if command -v timeout >/dev/null 2>&1; then exec timeout %d nice -n 10 sh -s; else exec nice -n 10 sh -s; fi", secs)
	if err := sess.Start(remote); err != nil {
		return errs.Wrap(errs.Runtime, err, "ssh start")
	}
	done := make(chan error, 1)
	go func() { err := sess.Wait(); pw.Close(); done <- err }()

	maxLines := lim.Lines
	if limit > 0 && (maxLines <= 0 || limit < maxLines) {
		maxLines = limit
	}
	if maxLines <= 0 {
		maxLines = connector.DefaultLimits.Lines
	}
	maxBytes := lim.Bytes
	if maxBytes <= 0 {
		maxBytes = connector.DefaultLimits.Bytes
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				pr.CloseWithError(ctx.Err())
				return
			}
		}
	}()
	n, b, truncated := 0, 0, false
	for {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
			return errs.New(errs.Timeout, "ssh command exceeded %s", lim.Timeout).WithHint("raise limits.timeout or make the command cheaper")
		case l, ok := <-lines:
			if !ok {
				err := <-done
				if truncated {
					return nil
				}
				var ee *ssh.ExitError
				if errors.As(err, &ee) {
					if ee.ExitStatus() == 124 {
						return errs.New(errs.Timeout, "ssh command exceeded %s (remote timeout)", lim.Timeout)
					}
					return errs.New(errs.Runtime, "command exited with status %d", ee.ExitStatus())
				}
				if err != nil {
					return errs.Wrap(errs.Runtime, err, "ssh command")
				}
				return nil
			}
			if truncated {
				continue // drain so the session can finish
			}
			n++
			b += len(l) + 1
			if n > maxLines || b > maxBytes {
				truncated = true
				_ = e.Text(fmt.Sprintf("… output truncated at %d lines / %d bytes", n-1, b-len(l)-1))
				_ = sess.Signal(ssh.SIGKILL)
				_ = sess.Close()
				continue
			}
			if err := e.Text(l); err != nil {
				truncated = true
				_ = sess.Close()
				if !errors.Is(err, connector.ErrLimit) {
					return err
				}
			}
		}
	}
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// ---- dialing ----

// Dial opens an authenticated ssh client to hop, dialing through parent.
func Dial(ctx context.Context, t *connector.Target, parent connector.Dialer) (*ssh.Client, error) {
	host, err := t.String(ctx, "host")
	if err != nil {
		return nil, err
	}
	port, err := t.StringOr(ctx, "port", "22")
	if err != nil {
		return nil, err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, errs.New(errs.Config, "port must be a number")
	}
	cfg, err := clientConfig(ctx, t, host)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(host, port)
	if parent == nil {
		parent = connector.DirectDialer()
	}
	dctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	nc, err := parent.DialContext(dctx, "tcp", addr)
	if err != nil {
		return nil, errs.Wrap(errs.Unreachable, err, "ssh dial %s", addr)
	}
	// make the handshake honour ctx
	stop := context.AfterFunc(dctx, func() { _ = nc.SetDeadline(time.Now()) })
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	stop()
	if err != nil {
		nc.Close()
		return nil, classify(err)
	}
	_ = nc.SetDeadline(time.Time{})
	return ssh.NewClient(cc, chans, reqs), nil
}

func classify(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "no supported methods remain"):
		return errs.AuthError(errors.New("ssh: server rejected the credential"))
	case strings.Contains(msg, "knownhosts: key mismatch"), strings.Contains(msg, "host key mismatch"):
		return errs.New(errs.PolicyBlocked, "ssh host key mismatch").WithHint("the server key differs from host_key/known_hosts — verify before updating the config")
	case strings.Contains(msg, "knownhosts: key is unknown"):
		return errs.New(errs.Config, "ssh host key unknown").WithHint("add the host to known_hosts or set host_key")
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(msg, "i/o timeout"):
		return errs.Wrap(errs.Timeout, err, "ssh handshake")
	}
	return errs.Wrap(errs.Unreachable, err, "ssh handshake")
}

// field resolves "<mode>.<k>" then "<k>", reporting whether it exists.
func field(ctx context.Context, t *connector.Target, k string, file bool) ([]byte, bool, error) {
	p := string(t.Mode) + "." + k
	if !t.Has(p) {
		if !t.Has(k) {
			return nil, false, nil
		}
		p = k
	}
	if raw, _ := t.Raw(p); file {
		if s, ok := raw.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "${file:") {
			b, err := t.File(ctx, p)
			return b, true, err
		}
	}
	s, err := t.String(ctx, p)
	return []byte(s), true, err
}

func clientConfig(ctx context.Context, t *connector.Target, host string) (*ssh.ClientConfig, error) {
	user, ok, err := field(ctx, t, "user", false)
	if err != nil {
		return nil, err
	}
	if !ok || len(user) == 0 {
		return nil, errs.New(errs.Config, "ssh: user is required")
	}
	var auths []ssh.AuthMethod
	key, ok, err := field(ctx, t, "key", true)
	if err != nil {
		return nil, err
	}
	if ok && len(bytes.TrimSpace(key)) > 0 {
		pass, hasPass, err := field(ctx, t, "passphrase", false)
		if err != nil {
			return nil, err
		}
		var signer ssh.Signer
		if hasPass {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, pass)
		} else {
			signer, err = ssh.ParsePrivateKey(key)
		}
		if err != nil {
			// never include key material
			return nil, errs.New(errs.Config, "ssh: key is not a valid private key (or wrong passphrase)")
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	pw, ok, err := field(ctx, t, "password", false)
	if err != nil {
		return nil, err
	}
	if ok && len(pw) > 0 {
		auths = append(auths, ssh.Password(string(pw)))
	}
	if len(auths) == 0 {
		return nil, errs.New(errs.Config, "ssh: set key or password")
	}
	hk, err := hostKeyCallback(ctx, t)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{User: string(user), Auth: auths, HostKeyCallback: hk, Timeout: 15 * time.Second,
		ClientVersion: "SSH-2.0-tmvp"}, nil
}

func hostKeyCallback(ctx context.Context, t *connector.Target) (ssh.HostKeyCallback, error) {
	if hk, ok, err := field(ctx, t, "host_key", false); err != nil {
		return nil, err
	} else if ok && len(bytes.TrimSpace(hk)) > 0 {
		var keys []ssh.PublicKey
		rest := hk
		for len(bytes.TrimSpace(rest)) > 0 {
			k, _, _, r, err := ssh.ParseAuthorizedKey(rest)
			if err != nil {
				// "host ssh-ed25519 AAAA" (known_hosts style line)? try known_hosts parser
				_, _, k2, _, r2, err2 := ssh.ParseKnownHosts(rest)
				if err2 != nil {
					return nil, errs.New(errs.Config, "ssh: host_key is not a valid public key line")
				}
				k, r = k2, r2
			}
			keys = append(keys, k)
			rest = r
		}
		return func(_ string, _ net.Addr, key ssh.PublicKey) error {
			for _, k := range keys {
				if bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil
				}
			}
			return errors.New("ssh: host key mismatch")
		}, nil
	}
	if kh, ok, err := field(ctx, t, "known_hosts", true); err != nil {
		return nil, err
	} else if ok && len(bytes.TrimSpace(kh)) > 0 {
		return knownHostsCallback(kh)
	}
	if v, ok := t.Raw("insecure"); ok && (v == true || v == "true") {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // explicit opt-in via config
	}
	return nil, errs.New(errs.Config, "ssh: no host key verification configured").
		WithHint("set host_key or known_hosts (or insecure: true, explicitly)")
}

// knownHostsCallback builds a callback from known_hosts content in memory.
func knownHostsCallback(content []byte) (ssh.HostKeyCallback, error) {
	type entry struct {
		marker string
		hosts  []string
		key    ssh.PublicKey
	}
	var ents []entry
	rest := content
	for len(bytes.TrimSpace(rest)) > 0 {
		marker, hosts, key, _, r, err := ssh.ParseKnownHosts(rest)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errs.New(errs.Config, "ssh: known_hosts content is invalid")
		}
		ents = append(ents, entry{marker, hosts, key})
		rest = r
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		addr := knownhosts.Normalize(hostname)
		matched := false
		for _, e := range ents {
			if !hostMatch(e.hosts, addr) {
				continue
			}
			if e.marker == "revoked" && bytes.Equal(e.key.Marshal(), key.Marshal()) {
				return errors.New("ssh: host key mismatch (revoked)")
			}
			if e.marker != "" {
				continue
			}
			matched = true
			if bytes.Equal(e.key.Marshal(), key.Marshal()) {
				return nil
			}
		}
		if matched {
			return errors.New("knownhosts: key mismatch")
		}
		return errors.New("knownhosts: key is unknown")
	}, nil
}

// hostMatch matches normalized addr against known_hosts patterns (plain, [h]:p, *, ?, !neg, hashed).
func hostMatch(patterns []string, addr string) bool {
	ok := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		var m bool
		if strings.HasPrefix(p, "|1|") {
			m = hashedMatch(p, addr)
		} else {
			m = wildcard(knownhosts.Normalize(p), addr) || wildcard(p, addr)
		}
		if m && neg {
			return false
		}
		if m {
			ok = true
		}
	}
	return ok
}

// hashedMatch checks a "|1|salt|hash" pattern: hash = HMAC-SHA1(salt, addr).
func hashedMatch(p, addr string) bool {
	parts := strings.Split(p, "|")
	if len(parts) != 4 {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	want, err2 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	m := hmac.New(sha1.New, salt)
	m.Write([]byte(addr))
	return hmac.Equal(m.Sum(nil), want)
}

func wildcard(p, s string) bool {
	if p == "" {
		return s == ""
	}
	switch p[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if wildcard(p[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && wildcard(p[1:], s[1:])
	}
	return s != "" && p[0] == s[0] && wildcard(p[1:], s[1:])
}
