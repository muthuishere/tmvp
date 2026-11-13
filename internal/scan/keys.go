// Package scan reads a repo for discovery without ever returning a secret
// value: only file paths, key names and whether a value is set.
package scan

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Key is one variable name found in an env file. The value is never kept.
type Key struct {
	File string `json:"file"` // repo-relative
	Env  string `json:"env"`  // from the file name: .env.production → production ("" for .env)
	Name string `json:"key"`
	Set  bool   `json:"set"` // a non-empty value is present
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"dist": true, "build": true, "target": true, ".next": true, ".terraform": true, "__pycache__": true}

var keyRe = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*=\s*(.*)$`)

// IsEnvFile: .env, .env.<x>, <x>.env (templates included: .env.example names keys too).
func IsEnvFile(name string) bool {
	return name == ".env" || strings.HasPrefix(name, ".env.") || strings.HasSuffix(name, ".env")
}

// EnvName derives the environment from an env file name.
func EnvName(name string) string {
	switch {
	case name == ".env":
		return ""
	case strings.HasPrefix(name, ".env."):
		return strings.TrimPrefix(name, ".env.")
	default:
		return strings.TrimSuffix(strings.TrimSuffix(name, ".env"), ".")
	}
}

// EnvKeys walks root and returns the key names of every env file, sorted by
// file then key. Values are read only to decide Set and are discarded.
func EnvKeys(root string) ([]Key, error) {
	var out []Key
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !IsEnvFile(d.Name()) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		rel, _ := filepath.Rel(root, p)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			m := keyRe.FindStringSubmatch(sc.Text())
			if m == nil {
				continue
			}
			v := dotenvValue(m[2])
			out = append(out, Key{File: filepath.ToSlash(rel), Env: EnvName(d.Name()), Name: m[1], Set: v != ""})
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Name < out[j].Name
	})
	return out, err
}

// dotenvValue unquotes a raw dotenv value and drops a trailing " # comment".
func dotenvValue(raw string) string {
	v := strings.TrimSpace(raw)
	if v != "" && (v[0] == '"' || v[0] == '\'') {
		if i := strings.IndexByte(v[1:], v[0]); i >= 0 {
			return v[1 : i+1]
		}
		return v
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// ResolveDotenv makes a $dotenv path absolute:
//   - absolute paths and ~/… are used as is;
//   - "./x" or "../x" are relative to the config file's directory (dir);
//   - any other relative path is relative to the repo root (nearest ancestor
//     of dir holding .git); if the file is not there, dir is tried next.
func ResolveDotenv(dir, file string) string {
	file = portablePath(file)
	if filepath.IsAbs(file) || isWindowsAbs(file) {
		return file
	}
	if strings.HasPrefix(file, "~/") || strings.HasPrefix(file, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, file[2:])
		}
	}
	if strings.HasPrefix(file, "./") || strings.HasPrefix(file, "../") || strings.HasPrefix(file, `.\`) || strings.HasPrefix(file, `..\`) {
		return filepath.Join(dir, file)
	}
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			p := filepath.Join(d, file)
			if _, err := os.Stat(p); err != nil {
				if _, err2 := os.Stat(filepath.Join(dir, file)); err2 == nil {
					return filepath.Join(dir, file)
				}
			}
			return p
		}
		parent := filepath.Dir(d)
		if parent == d {
			return filepath.Join(dir, file)
		}
		d = parent
	}
}

// DotenvValue reads one key from a dotenv file. Errors name the file and
// key, never a value. The last definition of a key wins (dotenv convention).
func DotenvValue(path, key string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("dotenv file %s: %w", path, err)
	}
	defer f.Close()
	var val string
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if m := keyRe.FindStringSubmatch(sc.Text()); m != nil && m[1] == key {
			val, found = dotenvValue(m[2]), true
		}
	}
	if !found {
		return nil, fmt.Errorf("key %s not in %s", key, path)
	}
	if val == "" {
		return nil, fmt.Errorf("key %s is empty in %s", key, path)
	}
	return []byte(val), nil
}

// portablePath lets one config work on every OS: "/" and "\\" are both
// accepted as separators and turned into this OS's separator, so
// "./secrets/.env.prod" and ".\\secrets\\.env.prod" mean the same file on
// macOS, Linux and Windows. A Windows drive path (C:\\… or C:/…) or UNC path
// (\\\\server\\share) is kept as an absolute path.
func portablePath(p string) string {
	if isWindowsAbs(p) && filepath.Separator == '/' {
		return p // a Windows-only path on macOS/Linux: leave it, it will not be found
	}
	return filepath.FromSlash(strings.ReplaceAll(p, `\`, "/"))
}

func isWindowsAbs(p string) bool {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}
