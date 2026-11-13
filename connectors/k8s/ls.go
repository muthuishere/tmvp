package k8s

import (
	"context"
	"errors"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// lsKinds are listed by `ls`, in this order. Kinds the cluster lacks or RBAC
// forbids are skipped so one gap does not hide the rest.
var lsKinds = []string{"namespaces", "deployments", "statefulsets", "daemonsets", "cronjobs", "services", "ingresses"}

// kindEmit tags every row with the kind it came from.
type kindEmit struct {
	connector.Emit
	kind string
}

func (k kindEmit) Row(r connector.Row) error {
	out := connector.Row{"kind": k.kind}
	for key, v := range r {
		if key != "kind" {
			out[key] = v
		}
	}
	return k.Emit.Row(out)
}

func (k *K8s) ls(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := k.t.EmitSaved(e, "manifests", ".yaml"); err != nil {
		return err
	}
	for _, kind := range lsKinds {
		cc := c
		cc.Args = []string{kind}
		cc.Flags = map[string]string{}
		for fk, fv := range c.Flags {
			if fk != "o" {
				cc.Flags[fk] = fv
			}
		}
		err := k.getVerb(ctx, cc, kindEmit{e, kind})
		if errors.Is(err, connector.ErrLimit) {
			return nil
		}
		if err != nil && errs.Is(err, errs.NotFound, errs.PolicyBlocked) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
