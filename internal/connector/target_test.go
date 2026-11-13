package connector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavedAcrossAssetRoots(t *testing.T) {
	dir := t.TempDir()
	r1, r2 := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	w := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w(filepath.Join(r1, "db", "queries", "users.sql"), "/*---\ndescription: from a\n---*/\nselect 1")
	w(filepath.Join(r2, "db", "queries", "users.sql"), "---\ndescription: from b\n---\nselect 2")
	w(filepath.Join(r2, "db", "queries", "orders.sql"), "---\ndescription: only b\n---\nselect 3")
	tg := NewTarget(Target{System: "db", Dir: dir, AssetRoots: []string{r1, r2}}, StaticFields{})
	s, err := tg.LoadSaved("queries", "@users", ".sql")
	if err != nil || s.Body != "select 1" || s.Description() != "from a" {
		t.Fatalf("first root must win: %+v %v", s, err)
	}
	if s, err = tg.LoadSaved("queries", "orders", ".sql"); err != nil || !strings.HasPrefix(s.Path, r2) {
		t.Fatalf("second root: %+v %v", s, err)
	}
	if _, err := tg.LoadSaved("queries", "nope", ".sql"); err == nil {
		t.Fatal("missing item found")
	}
	if _, err := tg.LoadSaved("queries", "../x", ".sql"); err == nil {
		t.Fatal("path traversal accepted")
	}
	var c Collector
	if err := tg.EmitSaved(&c, "queries", ".sql"); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 2 || c.Rows[0]["name"] != "@orders" || c.Rows[1]["source"] != filepath.Join(r1, "db", "queries", "users.sql") {
		t.Fatalf("rows %v", c.Rows)
	}
	// default root = Dir/assets
	d := NewTarget(Target{System: "db", Dir: dir}, StaticFields{})
	if d.AssetsDir() != filepath.Join(dir, "assets", "db") {
		t.Fatal(d.AssetsDir())
	}
}

// Owner decision: writes use write.dsn when set, else the same dsn (a
// separate write user is optional; no warning when the read login can write).
func TestModeStringFallsBackToShared(t *testing.T) {
	ctx := t.Context()
	shared := StaticFields{"dsn": "FAKE_SHARED"}
	split := StaticFields{"dsn": "FAKE_SHARED", "write": map[string]any{"dsn": "FAKE_RW"}}
	for _, c := range []struct {
		f    StaticFields
		mode Mode
		want string
	}{{shared, ModeRead, "FAKE_SHARED"}, {shared, ModeWrite, "FAKE_SHARED"}, {split, ModeRead, "FAKE_SHARED"}, {split, ModeWrite, "FAKE_RW"}} {
		got, err := NewTarget(Target{Mode: c.mode}, c.f).ModeString(ctx, "dsn")
		if err != nil || got != c.want {
			t.Fatalf("%s: got %q %v, want %q", c.mode, got, err, c.want)
		}
	}
}
