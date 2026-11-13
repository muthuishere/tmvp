package k8s

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	single := map[string]any{"kind": "Secret", "type": "kubernetes.io/service-account-token",
		"data": map[string]any{"token": "RkFLRV9TRUNSRVRfMTIz"}, "stringData": map[string]any{"pw": "FAKE_SECRET_123"}}
	list := map[string]any{"kind": "SecretList", "items": []any{
		map[string]any{"kind": "Secret", "data": map[string]any{"k": "RkFLRV9TRUNSRVRfMTIz"}},
		map[string]any{"data": map[string]any{"k2": "FAKE_SECRET_123"}}, // API list items have no kind
		map[string]any{"kind": "ConfigMap", "data": map[string]any{"c": "visible"}},
	}}
	for _, o := range []map[string]any{single, list} {
		redactSecrets(o)
		b, _ := json.Marshal(o)
		s := string(b)
		if strings.Contains(s, "RkFLRV9TRUNSRVRfMTIz") || strings.Contains(s, "FAKE_SECRET_123") {
			t.Fatalf("secret leaked: %s", s)
		}
	}
	if single["data"].(map[string]any)["token"] != "***" {
		t.Fatal("key should be kept with masked value")
	}
	if list["items"].([]any)[2].(map[string]any)["data"].(map[string]any)["c"] != "visible" {
		t.Fatal("non-secret data must not be masked")
	}
}
