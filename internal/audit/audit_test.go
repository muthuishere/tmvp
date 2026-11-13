package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/internal/secret"
)

func TestIntentOutcomeRedacted(t *testing.T) {
	dir := t.TempDir()
	red := secret.NewRedactor()
	red.Add("FAKE_SECRET_123")
	l := New("demo", dir, red)
	id, err := l.WriteIntent(map[string]any{"op": "write", "env": "prod", "args": []string{"update x set p='FAKE_SECRET_123' where id=1"}, "reason": "t"})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.WriteOutcome(id, map[string]any{"op": "write", "outcome": "ok"})
	_ = l.Event("command", map[string]any{"op": "run", "outcome": "ok"})
	b, _ := os.ReadFile(filepath.Join(dir, FileName))
	if strings.Contains(string(b), "FAKE_SECRET_123") {
		t.Fatal("secret in audit log")
	}
	st, _ := os.Stat(filepath.Join(dir, FileName))
	if st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("perm %v", st.Mode())
	}
	recs, err := Read(dir, ReadOptions{WritesOnly: true})
	if err != nil || len(recs) != 2 || recs[0].Attributes["phase"] != "intent" || recs[1].TraceID != id {
		t.Fatalf("%v %+v", err, recs)
	}
	if recs[0].Resource["service.name"] != "tmvp" || recs[0].Resource["tmvp.product"] != "demo" {
		t.Fatal("resource")
	}
	all, _ := Read(dir, ReadOptions{})
	if len(all) != 3 {
		t.Fatal(len(all))
	}
}
