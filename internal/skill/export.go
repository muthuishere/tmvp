package skill

import (
	"fmt"
	"strings"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// PgServicePortBase is the lowest local port used for tunnelled postgres
// (see connector.LocalPort: the same convention `portforward open` uses).
const PgServicePortBase = connector.LocalPortBase

// plain returns a field's value only when it contains no secret and no
// process-env reference (those could carry a secret); vars are expanded.
func plain(c *config.Config, env string, s *config.System, path string) (string, bool) {
	v, ok := s.Get(path)
	if !ok {
		return "", false
	}
	str, ok := v.(string)
	if !ok {
		return fmt.Sprint(v), true
	}
	parts := strings.Split(path, ".")
	if config.SecretishKeys[strings.ToLower(parts[len(parts)-1])] {
		return "", false
	}
	for _, r := range config.Refs(str) {
		if r.Kind != config.RefVar {
			return "", false
		}
	}
	out, err := c.ExpandField(env, str, nil)
	if err != nil {
		return "", false
	}
	return out, true
}

func firstPlain(c *config.Config, env string, s *config.System, paths ...string) string {
	for _, p := range paths {
		if v, ok := plain(c, env, s, p); ok && v != "" {
			return v
		}
	}
	return ""
}

// secretRef names where a field's secret comes from — the config path, never
// the command or value.
func secretRef(c *config.Config, env string, s *config.System, path string) string {
	v, ok := s.Get(path)
	if !ok {
		return ""
	}
	str, _ := v.(string)
	for _, r := range config.Refs(str) {
		switch r.Kind {
		case config.RefSecret, config.RefFileSecret:
			if d := c.LookupSecret(env, r.Name); d != nil {
				return d.Origin
			}
			return "secrets." + r.Name
		case config.RefEnvVar:
			return "env var $" + r.Name
		}
	}
	return ""
}

func envList(c *config.Config, only string) ([]string, error) {
	if only == "" {
		return c.EnvOrder, nil
	}
	if c.Envs[only] == nil {
		return nil, errs.New(errs.NotFound, "no env %q (have %s)", only, strings.Join(c.EnvOrder, ", "))
	}
	return []string{only}, nil
}

// PgService renders pg_service.conf entries for every postgres system.
// Passwords never appear: libpq gets them from PGPASSWORD / ~/.pgpass, which
// the user fills from their secret manager.
func PgService(c *config.Config, only string) (string, error) {
	envs, err := envList(c, only)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# tmvp export pg-service — %s. Contains NO passwords.\n", c.Product)
	b.WriteString("# Append to ~/.pg_service.conf (or point PGSERVICEFILE at a file), then: psql service=<name>\n")
	b.WriteString("# Password: libpq reads PGPASSWORD or ~/.pgpass — supply it from your secret manager at call time,\n")
	b.WriteString("#   e.g. sec run <NAME> -- psql service=<name>   (tmvp never exports secret values)\n")
	n := 0
	for _, en := range envs {
		e := c.Envs[en]
		for _, sn := range e.SystemOrder {
			s := e.Systems[sn]
			if s.Type != "postgres" {
				continue
			}
			n++
			fmt.Fprintf(&b, "\n[%s-%s-%s]\n", c.Product, en, sn)
			fmt.Fprintf(&b, "# %s.%s (postgres)", en, sn)
			if ref := secretRef(c, en, s, "read.dsn"); ref != "" {
				fmt.Fprintf(&b, " — read credential: %s", ref)
			}
			b.WriteString("\n")
			host := firstPlain(c, en, s, "host", "read.host")
			port := firstPlain(c, en, s, "port", "read.port")
			if len(s.Via) > 0 {
				last := s.Via[len(s.Via)-1]
				if hs := c.System(en, last); hs != nil && hs.Type == "portforward" {
					// same port `tmvp <env> <hop> open` listens on (local_port: or connector.LocalPort)
					lp := firstPlain(c, en, hs, "local_port")
					if lp == "" {
						lp = connector.LocalPortString(c.Product, en, last)
					}
					fmt.Fprintf(&b, "# via %s: first run `tmvp %s %s open` (listens on 127.0.0.1:%s → %s)\n",
						strings.Join(s.Via, " -> "), en, last, lp, orUnknown(hostPort(c, en, last, host, port)))
					host, port = "127.0.0.1", lp
				} else {
					lp := connector.LocalPortString(c.Product, en, sn)
					fmt.Fprintf(&b, "# via %s: plain psql does not ride tmvp's tunnel; add a portforward system (remote: %s, via: %s)\n#   and run `tmvp %s <it> open`, or forward %s to 127.0.0.1:%s yourself\n",
						strings.Join(s.Via, " -> "), orUnknown(hostPort(c, en, last, host, port)), last, en, orUnknown(hostPort(c, en, last, host, port)), lp)
					host, port = "127.0.0.1", lp
				}
			}
			if host != "" {
				fmt.Fprintf(&b, "host=%s\n", host)
			} else {
				b.WriteString("# host=  (lives in the DSN secret; fill it in)\n")
			}
			if port != "" {
				fmt.Fprintf(&b, "port=%s\n", port)
			}
			if v := firstPlain(c, en, s, "dbname", "database", "read.dbname", "read.database"); v != "" {
				fmt.Fprintf(&b, "dbname=%s\n", v)
			} else {
				b.WriteString("# dbname=\n")
			}
			if v := firstPlain(c, en, s, "read.user", "user"); v != "" {
				fmt.Fprintf(&b, "user=%s\n", v)
			} else {
				b.WriteString("# user=  (read-only user)\n")
			}
			b.WriteString("options=-c default_transaction_read_only=on\n")
		}
	}
	if n == 0 {
		return "", errs.New(errs.NotFound, "no postgres systems%s", inEnv(only))
	}
	return b.String(), nil
}

func hostPort(c *config.Config, env, hop, host, port string) string {
	if s := c.System(env, hop); s != nil && s.Type == "portforward" {
		if r, ok := plain(c, env, s, "remote"); ok {
			return r
		}
	}
	if host == "" {
		return ""
	}
	if port == "" {
		port = "5432"
	}
	return host + ":" + port
}

func orUnknown(s string) string {
	if s == "" {
		return "the db host"
	}
	return s
}

func inEnv(only string) string {
	if only == "" {
		return " in config"
	}
	return " in env " + only
}

// Kubeconfig renders a kubeconfig contexts fragment + kubectl aliases for
// every k8s system. The kubeconfig itself is a secret and is never included.
func Kubeconfig(c *config.Config, only string) (string, error) {
	envs, err := envList(c, only)
	if err != nil {
		return "", err
	}
	var b, aliases strings.Builder
	fmt.Fprintf(&b, "# tmvp export kubeconfig — %s. Contains NO credentials.\n", c.Product)
	b.WriteString("# Clusters and users come from each system's kubeconfig secret (named below); fetch it with\n")
	b.WriteString("# that secret's command at call time, e.g. kubectl --kubeconfig <(<command>) — never write it to disk.\n")
	b.WriteString("# Merge this fragment with KUBECONFIG=<secret kubeconfig>:<this file> to get product-named contexts.\n")
	b.WriteString("apiVersion: v1\nkind: Config\ncontexts:\n")
	n := 0
	for _, en := range envs {
		e := c.Envs[en]
		names := e.SystemOrder
		for _, sn := range names {
			s := e.Systems[sn]
			if s.Type != "k8s" {
				continue
			}
			n++
			ctx := firstPlain(c, en, s, "context")
			ns := firstPlain(c, en, s, "namespace")
			name := fmt.Sprintf("%s-%s-%s", c.Product, en, sn)
			fmt.Fprintf(&b, "# %s.%s — kubeconfig: %s", en, sn, orDash(secretRef(c, en, s, "kubeconfig")))
			if len(s.Via) > 0 {
				fmt.Fprintf(&b, "; reached via %s (forward the API server locally and set its server: to 127.0.0.1)", strings.Join(s.Via, " -> "))
			}
			b.WriteString("\n")
			fmt.Fprintf(&b, "- name: %s\n  context:\n", name)
			src := ctx
			if src == "" {
				src = "REPLACE_ME"
			}
			fmt.Fprintf(&b, "    cluster: %s   # cluster of context %q in the secret kubeconfig\n", src, src)
			fmt.Fprintf(&b, "    user: %s      # user of context %q in the secret kubeconfig\n", src, src)
			if ns != "" {
				fmt.Fprintf(&b, "    namespace: %s\n", ns)
			}
			fmt.Fprintf(&aliases, "#   alias k-%s-%s='kubectl --context %s", en, sn, src)
			if ns != "" {
				fmt.Fprintf(&aliases, " --namespace %s", ns)
			}
			aliases.WriteString("'\n")
		}
	}
	if n == 0 {
		return "", errs.New(errs.NotFound, "no k8s systems%s", inEnv(only))
	}
	b.WriteString("# Shell aliases (with the secret kubeconfig on KUBECONFIG):\n")
	b.WriteString(aliases.String())
	return b.String(), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
