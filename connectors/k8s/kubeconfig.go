package k8s

import (
	"net/url"
	"sort"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/muthuishere/tmvp/internal/errs"
)

// restConfig is a resolved context: where to connect and how to authenticate.
// The kubeconfig is parsed from bytes in memory by client-go's clientcmd; it
// is never written to disk. Exec credential plugins, client certs, tokens and
// basic auth are all handled by client-go's transport.
type restConfig struct {
	Context   string
	Cluster   string
	User      string
	Namespace string
	Server    *url.URL
	AuthKind  string // token | cert | basic | exec | none
	cfg       *rest.Config
}

// parseKubeconfig resolves context ctxName ("" = current-context).
func parseKubeconfig(b []byte, ctxName string) (*restConfig, error) {
	raw, err := clientcmd.Load(b)
	if err != nil {
		// never echo the parse error: it can quote kubeconfig content
		return nil, errs.New(errs.Config, "kubeconfig is not valid YAML")
	}
	if ctxName == "" {
		ctxName = raw.CurrentContext
	}
	if ctxName == "" && len(raw.Contexts) == 1 {
		for n := range raw.Contexts {
			ctxName = n
		}
	}
	kc, ok := raw.Contexts[ctxName]
	if !ok || kc == nil {
		names := make([]string, 0, len(raw.Contexts))
		for n := range raw.Contexts {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, errs.New(errs.Config, "kubeconfig has no context %q", ctxName).WithHint("contexts: " + strings.Join(names, ", "))
	}
	cl, ok := raw.Clusters[kc.Cluster]
	if !ok || cl == nil {
		return nil, errs.New(errs.Config, "kubeconfig has no cluster %q", kc.Cluster)
	}
	u, err := url.Parse(cl.Server)
	if err != nil || u.Host == "" {
		return nil, errs.New(errs.Config, "kubeconfig cluster %q has no valid server", kc.Cluster)
	}
	rc := &restConfig{Context: ctxName, Cluster: kc.Cluster, User: kc.AuthInfo, Namespace: kc.Namespace, Server: u, AuthKind: "none"}
	if ai := raw.AuthInfos[kc.AuthInfo]; ai != nil {
		switch {
		case ai.AuthProvider != nil:
			return nil, errs.New(errs.Unsupported, "kubeconfig user %q uses auth-provider (deprecated)", kc.AuthInfo).
				WithHint("use an exec credential plugin (e.g. gke-gcloud-auth-plugin) or a token")
		case ai.Exec != nil:
			rc.AuthKind = "exec"
			// tmvp is non-interactive: never let a plugin prompt on our stdin.
			ai.Exec.InteractiveMode = clientcmdapi.NeverExecInteractiveMode
		case ai.Token != "" || ai.TokenFile != "":
			rc.AuthKind = "token"
		case ai.Username != "":
			rc.AuthKind = "basic"
		case len(ai.ClientCertificateData) > 0 || ai.ClientCertificate != "":
			rc.AuthKind = "cert"
		}
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, ctxName, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, errs.New(errs.Config, "kubeconfig context %q is invalid: %s", ctxName, scrubHost(err.Error(), u))
	}
	if cfg.ExecProvider != nil {
		cfg.ExecProvider.StdinUnavailable = true
	}
	rc.cfg = cfg
	return rc, nil
}

// scrubHost removes the API server address from a message.
func scrubHost(msg string, u *url.URL) string {
	if u == nil || u.Host == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, u.String(), "<server>")
	msg = strings.ReplaceAll(msg, u.Host, "<server>")
	if h := u.Hostname(); h != "" {
		msg = strings.ReplaceAll(msg, h, "<server>")
	}
	return msg
}
