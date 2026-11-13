package skill

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"
)

func TestInstallAndRefresh(t *testing.T) {
	fsys := fstest.MapFS{
		"skills/disco/SKILL.md":        {Data: []byte("---\nname: disco\n---\nv1")},
		"skills/disco/references/a.md": {Data: []byte("ref")},
	}
	root := t.TempDir()
	if ch, err := Install(fsys, "disco", root); err != nil || !ch {
		t.Fatalf("first install: %v %v", ch, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "disco", "references", "a.md")); string(b) != "ref" {
		t.Fatal("reference not copied")
	}
	if ch, _ := Install(fsys, "disco", root); ch {
		t.Fatal("unchanged skill rewritten")
	}
	fsys["skills/disco/SKILL.md"] = &fstest.MapFile{Data: []byte("---\nname: disco\n---\nv2")}
	delete(fsys, "skills/disco/references/a.md")
	if ch, err := Install(fsys, "disco", root); err != nil || !ch {
		t.Fatalf("update: %v %v", ch, err)
	}
	if _, err := os.Stat(filepath.Join(root, "disco", "references", "a.md")); err == nil {
		t.Fatal("stale file kept after update")
	}
	// someone's own folder is never overwritten
	own := filepath.Join(root, "mine")
	os.MkdirAll(own, 0o755)
	fsys["skills/mine/SKILL.md"] = &fstest.MapFile{Data: []byte("x")}
	if _, err := Install(fsys, "mine", root); err == nil {
		t.Fatal("overwrote a folder tmvp did not write")
	}
	// a symlink (dev checkout) is left alone
	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "linked")
		os.Symlink(t.TempDir(), link)
		fsys["skills/linked/SKILL.md"] = &fstest.MapFile{Data: []byte("x")}
		if ch, err := Install(fsys, "linked", root); err != nil || ch {
			t.Fatalf("symlink touched: %v %v", ch, err)
		}
	}
	// Refresh updates only where tmvp installed before
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	Refresh(fsys)
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "disco")); err == nil {
		t.Fatal("refresh installed a skill the user never installed")
	}
	cl := filepath.Join(home, ".claude", "skills")
	if _, err := Install(fstest.MapFS{"skills/disco/SKILL.md": {Data: []byte("old")}}, "disco", cl); err != nil {
		t.Fatal(err)
	}
	Refresh(fsys)
	if b, _ := os.ReadFile(filepath.Join(cl, "disco", "SKILL.md")); string(b) != "---\nname: disco\n---\nv2" {
		t.Fatalf("refresh did not update: %q", b)
	}
}

func TestShippedDiscoverer(t *testing.T) {
	// the real embedded skill: present, has front-matter and both references
	fsys := os.DirFS(filepath.Join("..", ".."))
	names, err := Shipped(fsys)
	if err != nil || len(names) == 0 || names[0] != "tmvp-discoverer" {
		t.Fatalf("shipped %v %v", names, err)
	}
	for _, p := range []string{"SKILL.md", "references/evidence.md", "references/wiring.md"} {
		if _, err := os.Stat(filepath.Join("..", "..", "skills", "tmvp-discoverer", p)); err != nil {
			t.Error(err)
		}
	}
}
