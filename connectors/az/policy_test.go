package az

import (
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
)

func TestPolicySecretOps(t *testing.T) {
	for _, bad := range []string{"aks get-credentials -n c -g g", "keyvault secret show --name s --vault-name v",
		"storage account keys list -n a", "account get-access-token", "webapp deployment list-publishing-credentials -n w -g g",
		"acr credential show -n r", "cosmosdb keys list -n c -g g", "storage account show-connection-string -n a",
		"datafactory integration-runtime list-auth-keys", "redis list-keys -n r -g g"} {
		if v, _ := Policy.Classify(strings.Fields(bad)); v != cli.RevealsSecret {
			t.Errorf("%s: want RevealsSecret, got %v", bad, v)
		}
	}
	if v, _ := Policy.Classify(strings.Fields("aks show -n c -g g")); v != cli.ReadOnly {
		t.Errorf("aks show should be a read, got %v", v)
	}
}

func TestPolicyCredentialMetadata(t *testing.T) {
	for _, ok := range []string{"ad app credential list --id 00000000-0000-0000-0000-000000000000",
		"ad sp credential list --id fake-id", "AD APP CREDENTIAL LIST --id x"} {
		pos := cli.Positional(strings.Fields(ok), boolFlags)
		if v, _ := classify(pos); v != cli.ReadOnly {
			t.Errorf("%s: want ReadOnly, got %v", ok, v)
		}
	}
	for _, bad := range []string{"ad app credential reset --id x", "ad sp credential reset --id x",
		"ad app credential delete --id x --key-id k", "acr credential show -n r", "acr credential renew -n r",
		"ad app credential list extra", "keyvault secret show --name s --vault-name v", "ad app credential create --id x"} {
		pos := cli.Positional(strings.Fields(bad), boolFlags)
		if v, _ := classify(pos); v == cli.ReadOnly {
			t.Errorf("%s: must not be ReadOnly", bad)
		}
	}
	a := &AZ{}
	if err := a.Guard(connector.Call{Verb: "call", Args: strings.Fields("ad app credential list --id fake")}); err != nil {
		t.Errorf("guard blocked metadata list: %v", err)
	}
	if err := a.Guard(connector.Call{Verb: "call", Args: strings.Fields("ad app credential reset --id fake")}); err == nil {
		t.Error("guard allowed credential reset")
	}
}
