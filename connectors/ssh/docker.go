package ssh

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// dockerHelp is the allow-list, shown on errors.
const dockerHelp = "docker ps [-a] | logs <c> [--tail N] [--since X] [--timestamps] | inspect <c> | stats [c] | top <c> | images | compose ps | compose logs <svc> [--tail N] [--since X] [--timestamps] (compose: -F project=<dir> or -F file=<path>)"

func validName(s string) error {
	if !config.ValidContainerName(s) {
		return errs.New(errs.Usage, "bad container/service name %q (must match ^[A-Za-z0-9][A-Za-z0-9_.-]*$)", s)
	}
	return nil
}

func dockerDenied(format string, a ...any) error {
	return errs.New(errs.PolicyBlocked, format, a...).WithHint("allowed: " + dockerHelp +
		`; anything else is a write: tmvp write <env> <system> "docker ..." --reason "why"`)
}

// dockerArgv builds the argv for the read-only `docker` verb from the call's
// args (deny-by-default: only the allow-listed sub-commands and flags).
// since/limit come from the call (--since / --limit) and fill in for logs.
func dockerArgv(args []string, flags map[string]string, since time.Duration, limit int) ([]string, error) {
	if len(args) == 0 {
		return nil, errs.New(errs.Usage, "missing docker sub-command").WithHint("allowed: " + dockerHelp)
	}
	sub, rest := args[0], args[1:]
	// positional takes exactly n names, rejecting any flag.
	names := func(min, max int) ([]string, error) {
		if len(rest) < min || len(rest) > max {
			return nil, dockerDenied("docker %s: wrong number of arguments", sub)
		}
		for _, a := range rest {
			if strings.HasPrefix(a, "-") {
				return nil, dockerDenied("docker %s: flag %q not allowed", sub, a)
			}
			if err := validName(a); err != nil {
				return nil, err
			}
		}
		return rest, nil
	}
	switch sub {
	case "ps":
		argv := []string{"docker", "ps"}
		all := flags["all"] == "true"
		for _, a := range rest {
			if a != "-a" && a != "--all" {
				return nil, dockerDenied("docker ps: argument %q not allowed (only -a)", a)
			}
			all = true
		}
		if all {
			argv = append(argv, "-a")
		}
		return argv, nil
	case "images":
		if _, err := names(0, 0); err != nil {
			return nil, err
		}
		return []string{"docker", "images"}, nil
	case "inspect", "top":
		n, err := names(1, 1)
		if err != nil {
			return nil, err
		}
		return []string{"docker", sub, n[0]}, nil
	case "stats":
		var n []string
		for _, a := range rest {
			if a == "--no-stream" {
				continue
			}
			n = append(n, a)
		}
		rest = n
		n, err := names(0, 1)
		if err != nil {
			return nil, err
		}
		return append([]string{"docker", "stats", "--no-stream"}, n...), nil
	case "logs":
		return logsArgv([]string{"docker", "logs"}, rest, since, limit)
	case "compose":
		if len(rest) == 0 {
			return nil, dockerDenied("docker compose: want ps or logs")
		}
		argv := []string{"docker", "compose"}
		for _, k := range []string{"project", "file"} {
			v := flags[k]
			if v == "" {
				continue
			}
			if strings.HasPrefix(v, "-") || strings.ContainsAny(v, "\x00\n") {
				return nil, errs.New(errs.Usage, "bad -F %s value", k)
			}
			if k == "project" {
				argv = append(argv, "--project-directory", v)
			} else {
				argv = append(argv, "-f", v)
			}
		}
		switch rest[0] {
		case "ps":
			if len(rest) > 1 {
				return nil, dockerDenied("docker compose ps: no extra arguments allowed")
			}
			return append(argv, "ps"), nil
		case "logs":
			return logsArgv(append(argv, "logs", "--no-color"), rest[1:], since, limit)
		}
		return nil, dockerDenied("docker compose %s: not allowed", rest[0])
	}
	return nil, dockerDenied("docker %s: not allowed", sub)
}

// logsArgv parses `<name> [--tail N] [--since X] [--timestamps]`.
func logsArgv(argv, rest []string, since time.Duration, limit int) ([]string, error) {
	var name, tail, sinceS string
	ts := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		k, v, hasV := strings.Cut(a, "=")
		switch k {
		case "--tail", "-n", "--since":
			if !hasV {
				if i+1 >= len(rest) {
					return nil, dockerDenied("logs: %s needs a value", k)
				}
				i++
				v = rest[i]
			}
			if k == "--since" {
				if !validSince(v) {
					return nil, errs.New(errs.Usage, "logs: bad --since %q (want e.g. 10m, 2h, 2026-11-11T10:00:00)", v)
				}
				sinceS = v
			} else {
				if n, err := strconv.Atoi(v); err != nil || n < 0 {
					return nil, errs.New(errs.Usage, "logs: bad --tail %q", v)
				}
				tail = v
			}
		case "--timestamps", "-t":
			ts = true
		default:
			if strings.HasPrefix(a, "-") {
				return nil, dockerDenied("logs: flag %q not allowed", a)
			}
			if name != "" {
				return nil, dockerDenied("logs: one container/service only")
			}
			if err := validName(a); err != nil {
				return nil, err
			}
			name = a
		}
	}
	if name == "" {
		return nil, errs.New(errs.Usage, "logs: missing container/service name")
	}
	if tail == "" && limit > 0 {
		tail = strconv.Itoa(limit)
	}
	if sinceS == "" && since > 0 {
		sinceS = strconv.Itoa(int(since.Seconds())) + "s"
	}
	if tail != "" {
		argv = append(argv, "--tail", tail)
	}
	if sinceS != "" {
		argv = append(argv, "--since", sinceS)
	}
	if ts {
		argv = append(argv, "--timestamps")
	}
	return append(argv, name), nil
}

func validSince(v string) bool {
	if v == "" || len(v) > 40 {
		return false
	}
	for _, r := range v {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune(":.-+", r)) {
			return false
		}
	}
	return true
}

// shJoin quotes every argv element for sh.
func shJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shQuote(a)
	}
	return strings.Join(q, " ")
}

// maskInspect masks Config.Env values (KEY=***) in `docker inspect` JSON.
// Output that does not parse is withheld rather than risk leaking secrets.
func maskInspect(out string) (string, error) {
	var objs []map[string]any
	if err := json.Unmarshal([]byte(out), &objs); err != nil {
		return "", errs.New(errs.Runtime, "docker inspect: unexpected output (withheld: it may hold secrets)")
	}
	for _, o := range objs {
		cfg, _ := o["Config"].(map[string]any)
		if cfg == nil {
			continue
		}
		env, _ := cfg["Env"].([]any)
		for i, e := range env {
			s, _ := e.(string)
			k, _, _ := strings.Cut(s, "=")
			env[i] = k + "=***"
		}
	}
	b, err := json.MarshalIndent(objs, "", "  ")
	if err != nil {
		return "", errs.Wrap(errs.Runtime, err, "docker inspect")
	}
	return string(b), nil
}

// inContainer wraps cmd as `docker exec <c> sh -c '<cmd>'`.
func inContainer(c, cmd string, sudo bool) string {
	return dockerPrefix(sudo) + "docker exec " + shQuote(c) + " sh -c " + shQuote(cmd)
}

// scriptInContainer pipes body into `docker exec -i <c> sh -s -- args` via a
// quoted here-doc (no expansion; the delimiter never occurs in body).
func scriptInContainer(c, body string, args []string, sudo bool) string {
	delim := "TMVP_SCRIPT_EOF"
	for strings.Contains(body, delim) {
		delim += "_X"
	}
	q := []string{"--"}
	for _, a := range args {
		q = append(q, shQuote(a))
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return dockerPrefix(sudo) + "docker exec -i " + shQuote(c) + " sh -s " + strings.Join(q, " ") +
		" <<'" + delim + "'\n" + body + delim + "\n"
}

func dockerPrefix(sudo bool) string {
	if sudo {
		return "sudo -n "
	}
	return ""
}

// capture runs cmd and returns its output lines (stdout+stderr).
func (s *SSH) capture(ctx context.Context, cmd string, timeout time.Duration) ([]string, error) {
	var out []string
	err := s.exec(ctx, cmd, connector.EmitFuncs{TextFn: func(l string) error { out = append(out, l); return nil }}, 100000, timeout)
	return out, err
}

// dockerErr turns docker's output into a clear error.
func dockerErr(out []string, err error, what string) error {
	text := strings.ToLower(strings.Join(out, "\n"))
	switch {
	case strings.Contains(text, "no such object"), strings.Contains(text, "no such container"):
		return errs.New(errs.NotFound, "%s: container not found", what).WithHint("tmvp <env> <ssh-system> docker ps -a")
	case strings.Contains(text, "permission denied") && strings.Contains(text, "docker"):
		return errs.New(errs.PolicyBlocked, "%s: the ssh user may not use docker", what).
			WithHint("add the ssh user to the docker group (sudo-less docker: usermod -aG docker <user>), or set sudo: true on the container hop")
	case strings.Contains(text, "not found") && strings.Contains(text, "docker"), strings.Contains(text, "command not found"):
		return errs.New(errs.Unsupported, "%s: docker is not installed on the ssh host", what)
	case strings.Contains(text, "a password is required"), strings.Contains(text, "sudo:"):
		return errs.New(errs.PolicyBlocked, "%s: sudo needs a password", what).WithHint("allow passwordless sudo for docker, or use the docker group instead of sudo: true")
	}
	return err
}

func (s *SSH) docker(ctx context.Context, c connector.Call, e connector.Emit) error {
	argv, err := dockerArgv(c.Args, c.Flags, c.Since, c.Limit)
	if err != nil {
		return err
	}
	sudo := c.Flags["sudo"] == "true"
	// the container name is argv's last element for these; resolve a prefix
	if sub := argv[1]; sub == "logs" || sub == "inspect" || sub == "top" || (sub == "stats" && len(argv) > 3) {
		n, err := s.resolveContainer(ctx, argv[len(argv)-1], sudo)
		if err != nil {
			return err
		}
		argv[len(argv)-1] = n
	}
	cmd := dockerPrefix(sudo) + shJoin(argv)
	to := connector.CallTimeout(c, s.t.Limits)
	if argv[1] != "inspect" {
		var out []string
		err := s.exec(ctx, cmd, connector.EmitFuncs{
			RowFn:  e.Row,
			TextFn: func(l string) error { out = append(out, l); return e.Text(l) },
		}, c.Limit, to)
		if err != nil {
			return dockerErr(out, err, "docker "+argv[1])
		}
		return nil
	}
	out, err := s.capture(ctx, cmd, to)
	if err != nil {
		return dockerErr(out, err, "docker inspect")
	}
	masked, err := maskInspect(strings.Join(out, "\n"))
	if err != nil {
		return err
	}
	for _, l := range strings.Split(masked, "\n") {
		if err := e.Text(l); err != nil {
			return ignoreLimit(err)
		}
	}
	return nil
}

// lsContainers emits one row per running container; silent when docker is
// absent or not permitted (ls must not fail on a plain host).
func (s *SSH) lsContainers(ctx context.Context, e connector.Emit) error {
	out, err := s.capture(ctx, "docker ps --format '{{json .}}' 2>/dev/null", 10*time.Second)
	if err != nil {
		return nil
	}
	for _, l := range out {
		var p struct{ Names, Image, Status, Ports string }
		if json.Unmarshal([]byte(l), &p) != nil || p.Names == "" {
			continue
		}
		if err := e.Row(connector.Row{"kind": "containers", "name": p.Names, "image": p.Image,
			"status": p.Status, "ports": p.Ports}); err != nil {
			return ignoreLimit(err)
		}
	}
	return nil
}

// ---- container hop ----

// ContainerTunneler opens a `{container: name}` hop on top of an ssh hop.
type ContainerTunneler struct{}

// resolveTTL bounds how long a container IP is reused (compose recreate changes it).
const resolveTTL = 3 * time.Second

// OpenTunnel needs parent to be an ssh tunnel (validate enforces the order).
func (ContainerTunneler) OpenTunnel(_ context.Context, hop *connector.Target, parent connector.Dialer) (connector.Tunnel, error) {
	st, ok := parent.(*Tunnel)
	if !ok {
		return nil, errs.New(errs.Config, "container hop %s must directly follow an ssh hop", hop.System)
	}
	raw := func(k string) string {
		v, _ := hop.Raw(k)
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	ct := &ContainerTunnel{ssh: st, name: raw("container"), network: raw("network"), sudo: raw("sudo") == "true"}
	if err := validName(ct.name); err != nil {
		return nil, err
	}
	if ct.network != "" {
		if err := validName(ct.network); err != nil {
			return nil, err
		}
	}
	return ct, nil
}

// ContainerTunnel dials <container ip>:<port of the dialed addr> through ssh.
type ContainerTunnel struct {
	ssh           *Tunnel
	name, network string
	sudo          bool

	mu    sync.Mutex
	ip    string
	until time.Time
	now   func() time.Time
}

// DialContext ignores the host of addr: it dials the container's IP on addr's port.
func (t *ContainerTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, errs.New(errs.Config, "container hop: address %q has no port", addr)
	}
	ip, err := t.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return t.ssh.DialContext(ctx, network, net.JoinHostPort(ip, port))
}

// Close is a no-op: the ssh tunnel underneath is owned (and closed) separately.
func (t *ContainerTunnel) Close() error { return nil }

func (t *ContainerTunnel) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *ContainerTunnel) resolve(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ip != "" && t.clock().Before(t.until) {
		return t.ip, nil
	}
	cmd := dockerPrefix(t.sudo) + "docker inspect -f " + shQuote("{{.State.Running}}|{{json .NetworkSettings.Networks}}") + " " + shQuote(t.name) + " 2>&1"
	s := &SSH{t: connector.NewTarget(connector.Target{}, nil), client: t.ssh.c}
	name, err := s.resolveContainer(ctx, t.name, t.sudo)
	if err != nil {
		return "", err
	}
	cmd = dockerPrefix(t.sudo) + "docker inspect -f " + shQuote("{{.State.Running}}|{{json .NetworkSettings.Networks}}") + " " + shQuote(name) + " 2>&1"
	out, err := s.capture(ctx, cmd, 15*time.Second)
	what := "container " + name
	if err != nil {
		return "", dockerErr(out, errs.Wrap(errs.Unreachable, err, "%s: docker inspect", what), what)
	}
	ip, err := pickIP(strings.Join(out, "\n"), t.network, what)
	if err != nil {
		return "", err
	}
	t.ip, t.until = ip, t.clock().Add(resolveTTL)
	return ip, nil
}

// pickIP parses "<running>|<networks json>" and returns the IP on network
// (or the first network by name when network is "").
func pickIP(out, network, what string) (string, error) {
	out = strings.TrimSpace(out)
	running, js, ok := strings.Cut(out, "|")
	if !ok {
		return "", dockerErr([]string{out}, errs.New(errs.Runtime, "%s: unexpected docker inspect output", what), what)
	}
	if strings.TrimSpace(running) != "true" {
		return "", errs.New(errs.Unreachable, "%s: container is not running", what).WithHint("start it, or check: tmvp <env> <ssh-system> docker ps -a")
	}
	var nets map[string]struct{ IPAddress, GlobalIPv6Address string }
	if err := json.Unmarshal([]byte(js), &nets); err != nil {
		return "", errs.New(errs.Runtime, "%s: cannot parse networks", what)
	}
	names := make([]string, 0, len(nets))
	for n := range nets {
		names = append(names, n)
	}
	sort.Strings(names)
	if network != "" {
		n, ok := nets[network]
		if !ok {
			return "", errs.New(errs.NotFound, "%s: not attached to network %q (has %s)", what, network, strings.Join(names, ", "))
		}
		if n.IPAddress == "" {
			return "", errs.New(errs.Unreachable, "%s: no IP on network %q", what, network)
		}
		return n.IPAddress, nil
	}
	for _, n := range names {
		if ip := nets[n].IPAddress; ip != "" {
			return ip, nil
		}
	}
	return "", errs.New(errs.Unreachable, "%s: has no container IP (network_mode host/none?)", what).
		WithHint("use the host-published port through the ssh hop instead")
}
