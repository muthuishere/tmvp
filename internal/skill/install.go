package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/muthuishere/tmvp/internal/errs"
)

// managedMarker marks a skill folder tmvp wrote; its content is the hash of
// what was written, so a refresh happens only when the shipped skill changed.
const managedMarker = ".tmvp-managed"

// InstallRoots are the user-level skill folders: ~/.agents/skills (Codex and
// other agents), ~/.claude/skills, and $CLAUDE_CONFIG_DIR/skills when set.
func InstallRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	roots := []string{filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".claude", "skills")}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		if r := filepath.Join(d, "skills"); filepath.Clean(r) != filepath.Clean(roots[1]) {
			roots = append(roots, r)
		}
	}
	return roots
}

// Shipped lists the skill names in fsys (top-level folders of skills/).
func Shipped(fsys fs.FS) ([]string, error) {
	ents, err := fs.ReadDir(fsys, "skills")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func digest(fsys fs.FS, name string) (string, error) {
	h := sha256.New()
	var files []string
	err := fs.WalkDir(fsys, path.Join("skills", name), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, p := range files {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", err
		}
		h.Write([]byte(p + "\x00"))
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Install writes skill name from fsys into root/name. A folder tmvp did not
// write (no marker) is someone's own work and is refused; a symlink (a dev
// checkout) is left alone. It reports whether anything changed.
func Install(fsys fs.FS, name, root string) (changed bool, err error) {
	want, err := digest(fsys, name)
	if err != nil {
		return false, err
	}
	dst := filepath.Join(root, name)
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return false, nil
		}
		got, err := os.ReadFile(filepath.Join(dst, managedMarker))
		if err != nil {
			return false, errs.New(errs.Usage, "%s exists and was not installed by tmvp; not overwriting", dst).
				WithHint("move it away, then: tmvp skill install")
		}
		if string(got) == want {
			return false, nil
		}
		if err := os.RemoveAll(dst); err != nil {
			return false, errs.Wrap(errs.Runtime, err, "replace %s", dst)
		}
	}
	src := path.Join("skills", name)
	err = fs.WalkDir(fsys, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.FromSlash(src), filepath.FromSlash(p))
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		return false, errs.Wrap(errs.Runtime, err, "install %s", dst)
	}
	if err := os.WriteFile(filepath.Join(dst, managedMarker), []byte(want), 0o644); err != nil {
		return false, errs.Wrap(errs.Runtime, err, "install %s", dst)
	}
	return true, nil
}

// Refresh re-installs shipped skills only where tmvp installed them before
// (marker present) and the shipped copy changed. Errors are ignored: it runs
// on every command and must never get in the way.
func Refresh(fsys fs.FS) {
	names, err := Shipped(fsys)
	if err != nil {
		return
	}
	for _, root := range InstallRoots() {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(root, n, managedMarker)); err == nil {
				_, _ = Install(fsys, n, root)
			}
		}
	}
}
