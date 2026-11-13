package skill

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
)

// LintIssue is one `tmvp skill lint` finding.
type LintIssue struct {
	Level string `json:"level"` // error | warn
	Path  string `json:"path"`
	Msg   string `json:"msg"`
}

// LintOptions plugs the connector registry in (cmd/tmvp passes it).
type LintOptions struct {
	Validate config.ValidateOptions
}

// savedKinds are the folders whose files must carry front-matter.
var savedKinds = []string{"queries", "scripts", "pages", "apis", "manifests"}

var (
	pemKey   = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	tmvpHead = regexp.MustCompile(`(?i)^\s*(#|//|--|;|rem\b|::|<#)\s*tmvp:`)
	urlRE    = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)
)

// Lint checks a skill's STRUCTURE only: SKILL.md + front-matter, config
// validates (incl. the literal-secret check), asset paths exist, every saved
// item has front-matter, no literal secret in any skill/asset file. It never
// runs a secret command and never connects anywhere.
func Lint(dir string, o LintOptions) []LintIssue {
	var is []LintIssue
	add := func(level, p, f string) { is = append(is, LintIssue{level, p, f}) }

	sk := filepath.Join(dir, "SKILL.md")
	if b, err := os.ReadFile(sk); err != nil {
		add("error", sk, "SKILL.md missing")
	} else {
		fm, _ := connector.SplitFrontMatter(string(b))
		var h struct{ Name, Description string }
		if fm == "" || yaml.Unmarshal([]byte(fm), &h) != nil {
			add("error", sk, "no YAML front-matter (--- name/description ---)")
		} else {
			if h.Name == "" {
				add("error", sk, "front-matter: name missing")
			}
			if h.Description == "" {
				add("error", sk, "front-matter: description missing")
			}
		}
		scanSecrets(sk, string(b), add)
	}

	cp := filepath.Join(dir, "config.yaml")
	cfg, err := config.Load(cp)
	if err != nil {
		add("error", cp, err.Error())
		return is
	}
	for _, v := range cfg.Validate(o.Validate) {
		add(v.Level, cp+": "+v.Path, v.Message)
	}
	for _, root := range cfg.AssetRoots() {
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			if len(cfg.Assets) > 0 {
				add("error", root, "asset path does not exist")
			}
			continue
		}
		lintAssets(root, add)
	}
	sort.SliceStable(is, func(i, j int) bool { return is[i].Level < is[j].Level })
	return is
}

func lintAssets(root string, add func(level, p, f string)) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			add("error", p, err.Error())
			return nil
		}
		s := string(b)
		scanSecrets(p, s, add)
		rel, _ := filepath.Rel(root, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		// <system>/<kind>/<file>
		if len(parts) == 3 && contains(savedKinds, parts[1]) {
			fm, _ := connector.SplitFrontMatter(s)
			if fm == "" && !(parts[1] == "scripts" && hasTmvpHeader(s)) {
				add("error", p, "saved item has no front-matter (--- description: … ---)")
			} else if fm != "" {
				var m map[string]any
				if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
					add("error", p, "front-matter is not valid YAML: "+err.Error())
				}
			}
		}
		return nil
	})
}

func hasTmvpHeader(s string) bool {
	sc := bufio.NewScanner(strings.NewReader(s))
	for i := 0; i < 15 && sc.Scan(); i++ {
		if tmvpHead.MatchString(sc.Text()) {
			return true
		}
	}
	return false
}

// scanSecrets flags private keys, URLs with user:password@ and
// `secret-ish-key: literal` lines (the config literal-secret check).
func scanSecrets(p, s string, add func(level, p, f string)) {
	if pemKey.MatchString(s) {
		add("error", p, "contains a private key block")
	}
	for i, line := range strings.Split(s, "\n") {
		for _, u := range urlRE.FindAllString(line, -1) {
			if why := config.LiteralSecret("url", u); why != "" {
				add("error", p+":"+strconv.Itoa(i+1), why)
			}
		}
		k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")), ":")
		if !ok || strings.ContainsAny(k, " \t{}") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if v == "" || strings.HasPrefix(v, "#") || strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") ||
			strings.HasPrefix(v, "$(") || strings.HasPrefix(v, "$file(") || strings.HasPrefix(v, "|") || strings.HasPrefix(v, ">") {
			continue
		}
		if config.SecretishKeys[strings.ToLower(k)] {
			if why := config.LiteralSecret(strings.ToLower(k), v); why != "" {
				add("error", p+":"+strconv.Itoa(i+1), why)
			}
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
