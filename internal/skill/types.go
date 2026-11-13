package skill

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// slotCtx names one system in one env, for building its config slots.
type slotCtx struct {
	Product, Env, Name, Type string
}

// secretCmd is the obvious placeholder command for an unfilled secret slot.
func (c slotCtx) secretCmd(suffix string) string {
	id := strings.ToUpper(strings.Join([]string{"REPLACE_ME", c.Product, c.Env, c.Name, suffix}, "_"))
	id = strings.NewReplacer("-", "_", ".", "_").Replace(id)
	return "$file(sec get " + id + " --out ${out})"
}

// secretName is an env-scoped named secret (for ${file:secrets.x} fields).
func (c slotCtx) secretName(suffix string) string {
	return strings.ReplaceAll(c.Name, "-", "_") + "_" + suffix
}

func (c slotCtx) where() string { return c.Env + "." + c.Name }

// built is one system's config node plus env-level secrets it needs.
type built struct {
	Node    *yaml.Node
	Secrets []pair // added to envs.<env>.secrets
}

type pair struct {
	K, Comment string
	V          *yaml.Node
}

func str(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	if strings.ContainsAny(s, "${}") {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}
func strc(s, comment string) *yaml.Node {
	n := str(s)
	n.LineComment = "# " + comment
	return n
}
func boolean(b bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(b)}
}

func mapping(ps ...pair) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, p := range ps {
		k := str(p.K)
		if p.Comment != "" {
			k.HeadComment = "# " + p.Comment
		}
		if strings.HasPrefix(p.V.LineComment, "# SECRET") { // long slot notes go above the key
			k.HeadComment, p.V.LineComment = p.V.LineComment, ""
		}
		n.Content = append(n.Content, k, p.V)
	}
	return n
}

func secretComment(what string, c slotCtx) string {
	return "SECRET: " + what + " for " + c.where() + " — replace with your secret manager's command (never a value)"
}

// savedDir is the saved-item folder people fill for a type ("" = none).
func savedDir(typ string) string {
	switch typ {
	case "postgres", "mysql", "mongo":
		return "queries"
	case "ssh", "script", "aws", "gcp", "az":
		return "scripts"
	case "k8s":
		return "manifests"
	case "web":
		return "pages"
	case "http":
		return "apis"
	}
	return ""
}

// buildSystem returns the config node for one system in one env.
func buildSystem(c slotCtx) built {
	t := pair{K: "type", V: str(c.Type)}
	switch c.Type {
	case "postgres", "mysql":
		return built{Node: mapping(t,
			pair{K: "read", V: mapping(pair{K: "dsn", V: strc(c.secretCmd("RO"), secretComment("read-only DSN ("+c.Type+"://user:pass@host:port/db)", c))})},
			pair{K: "write", V: mapping(pair{K: "dsn", V: strc(c.secretCmd("RW"), secretComment("read-write DSN", c))})},
		)}
	case "mongo":
		return built{Node: mapping(t,
			pair{K: "read", V: mapping(pair{K: "uri", V: strc(c.secretCmd("RO"), secretComment("read-only mongodb:// URI", c))})},
			pair{K: "write", V: mapping(pair{K: "uri", V: strc(c.secretCmd("RW"), secretComment("read-write mongodb:// URI", c))})},
		)}
	case "redis":
		return built{Node: mapping(t, pair{K: "url", V: strc(c.secretCmd("URL"), secretComment("redis:// URL incl. password", c))})}
	case "nats":
		return built{Node: mapping(t,
			pair{K: "url", V: strc("nats://REPLACE_ME:4222", "server URL (no credentials here)")},
			pair{K: "creds", V: strc(c.secretCmd("CREDS"), secretComment("NATS .creds content", c))},
		)}
	case "kafka":
		return built{Node: mapping(t,
			pair{K: "brokers", V: strc("REPLACE_ME:9092", "bootstrap brokers")},
			pair{K: "password", V: strc(c.secretCmd("PASSWORD"), secretComment("SASL password", c))},
		)}
	case "k8s":
		sn := c.secretName("kubeconfig")
		return built{
			Node: mapping(t,
				pair{K: "kubeconfig", V: str("${file:secrets." + sn + "}")},
				pair{K: "context", V: strc("REPLACE_ME", "kube context name inside that kubeconfig")},
				pair{K: "namespace", V: str("default")},
			),
			Secrets: []pair{{K: sn, V: strc(c.secretCmd("KUBECONFIG"), secretComment("kubeconfig file content", c))}},
		}
	case "ssh":
		sn := c.secretName("key")
		return built{
			Node: mapping(t,
				pair{K: "host", V: strc("REPLACE_ME", "hostname or IP")},
				pair{K: "user", V: str("REPLACE_ME")},
				pair{K: "key", V: str("${file:secrets." + sn + "}")},
			),
			Secrets: []pair{{K: sn, V: strc(c.secretCmd("SSH_KEY"), secretComment("ssh private key content", c))}},
		}
	case "portforward":
		return built{Node: mapping(t,
			pair{K: "remote", V: strc("REPLACE_ME:5432", "host:port reached from the hop; add via: <ssh-or-k8s system>")},
		)}
	case "gcp":
		sn := c.secretName("sa")
		return built{
			Node: mapping(t,
				pair{K: "project", V: str("REPLACE_ME")},
				pair{K: "credentials", V: str("${file:secrets." + sn + "}")},
				pair{K: "login", V: mapping(pair{K: "manual", V: str("gcloud auth login")}, pair{K: "check", V: str("gcloud auth print-access-token")})},
			),
			Secrets: []pair{{K: sn, V: strc(c.secretCmd("SA_JSON"), secretComment("service-account JSON", c))}},
		}
	case "aws":
		return built{Node: mapping(t,
			pair{K: "profile", V: str("REPLACE_ME")},
			pair{K: "region", V: str("REPLACE_ME")},
			pair{K: "login", V: mapping(pair{K: "manual", V: str("aws sso login --profile REPLACE_ME")}, pair{K: "check", V: str("aws sts get-caller-identity --profile REPLACE_ME")})},
		)}
	case "az":
		return built{Node: mapping(t,
			pair{K: "subscription", V: str("REPLACE_ME")},
			pair{K: "login", V: mapping(pair{K: "manual", V: str("az login")}, pair{K: "check", V: str("az account show")})},
		)}
	case "web":
		return built{Node: mapping(t,
			pair{K: "base_url", V: strc("https://REPLACE_ME", "app URL in "+c.Env)},
			pair{K: "users", V: mapping(pair{K: "ops", V: mapping(
				pair{K: "role", V: str("read")},
				pair{K: "user", V: str("REPLACE_ME")},
				pair{K: "password", V: strc(c.secretCmd("OPS_PASSWORD"), secretComment("login password of user ops", c))},
			)})},
		)}
	case "http":
		return built{Node: mapping(t,
			pair{K: "base_url", V: str("https://REPLACE_ME")},
			pair{K: "token", V: strc(c.secretCmd("TOKEN"), secretComment("API bearer token", c))},
		)}
	case "grpc":
		return built{Node: mapping(t,
			pair{K: "address", V: strc("REPLACE_ME:443", "host:port in "+c.Env)},
			pair{K: "tls", V: str("true")},
		)}
	case "prometheus", "loki", "elasticsearch":
		return built{Node: mapping(t, pair{K: "url", V: str("https://REPLACE_ME")})}
	case "git":
		return built{Node: mapping(t, pair{K: "url", V: str("git@REPLACE_ME")})}
	}
	return built{Node: mapping(t)}
}
