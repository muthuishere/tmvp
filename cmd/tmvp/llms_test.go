package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/internal/connector"
)

const (
	beginVerbs = "<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->"
	endVerbs   = "<!-- END verbs -->"
	docsRoot   = "../.."
)

// lsPending lists types that do not implement `ls` yet. TODO(k8s): remove
// once the client-go rewrite adds ls (namespaces/workloads).
var lsPending = map[string]bool{}

func TestEveryConnectorHasLs(t *testing.T) {
	for _, typ := range connector.Types() {
		v, ok := connector.FindVerb(connector.VerbsOf(typ), "ls")
		if lsPending[typ] || testOnlyTypes[typ] {
			continue
		}
		if !ok || v.Mode != connector.Read {
			t.Errorf("%s: missing read verb ls (CONTRACT §2.3)", typ)
		}
	}
}

// TestLLMSDocsInSync fails when docs/llms/types/*.md verb tables or
// llms-full.txt drift from the registry. TMVP_UPDATE_DOCS=1 rewrites them
// (`go generate ./...` at the repo root).
func TestLLMSDocsInSync(t *testing.T) {
	update := os.Getenv("TMVP_UPDATE_DOCS") != ""
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(beginVerbs) + `.*?` + regexp.QuoteMeta(endVerbs))
	for _, typ := range connector.Types() {
		if testOnlyTypes[typ] {
			continue
		}
		p := filepath.Join(docsRoot, "docs", "llms", "types", typ+".md")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s: no doc %s", typ, p)
			continue
		}
		want := beginVerbs + "\n" + VerbTableMarkdown(typ) + endVerbs
		if !re.Match(b) {
			t.Errorf("%s: verbs markers missing", p)
			continue
		}
		got := re.ReplaceAllLiteral(b, []byte(want))
		if !bytes.Equal(got, b) {
			if update {
				_ = os.WriteFile(p, got, 0o644)
			} else {
				t.Errorf("%s drifted from %s.Verbs() — run go generate ./...", p, typ)
			}
		}
	}
	full := buildFull(t)
	fp := filepath.Join(docsRoot, "llms-full.txt")
	old, _ := os.ReadFile(fp)
	if !bytes.Equal(old, full) {
		if update {
			_ = os.WriteFile(fp, full, 0o644)
		} else {
			t.Error("llms-full.txt is stale — run go generate ./...")
		}
	}
}

func buildFull(t *testing.T) []byte {
	idx, err := os.ReadFile(filepath.Join(docsRoot, "llms.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.Write(idx)
	var files []string
	for _, n := range []string{"overview", "install", "config", "writes", "code-mode", "errors", "server", "skills", "troubleshooting", "doctor", "environments", "secrets", "logs-and-metrics"} {
		files = append(files, filepath.Join("docs", "llms", n+".md"))
	}
	types, _ := filepath.Glob(filepath.Join(docsRoot, "docs", "llms", "types", "*.md"))
	sort.Strings(types)
	for _, p := range types {
		r, _ := filepath.Rel(docsRoot, p)
		files = append(files, r)
	}
	for _, f := range files {
		c, err := os.ReadFile(filepath.Join(docsRoot, f))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("\n\n---\n<!-- " + filepath.ToSlash(f) + " -->\n\n")
		b.Write(c)
	}
	return b.Bytes()
}

func TestLLMSIndexLinksExist(t *testing.T) {
	idx, _ := os.ReadFile(filepath.Join(docsRoot, "llms.txt"))
	for _, m := range regexp.MustCompile(`\]\((docs/llms/[^)]+)\)`).FindAllStringSubmatch(string(idx), -1) {
		if _, err := os.Stat(filepath.Join(docsRoot, m[1])); err != nil {
			t.Errorf("llms.txt links %s: %v", m[1], err)
		}
	}
	if !strings.HasPrefix(string(idx), "# tmvp\n\n> ") {
		t.Error("llms.txt must start with '# title' and a '> summary' (llmstxt.org)")
	}
}

func TestLLMHelpWithoutConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cases := map[string][]string{
		"":               {"tmvp (read dispatch)", "envsel", "related: tmvp llms overview"},
		"write":          {"# tmvp write", "--reason", "examples:", "related: tmvp llms writes"},
		"prod db query":  {"postgres query", "read (plain call", "related: tmvp llms postgres", "mysql query"},
		"verbs postgres": {"# tmvp verbs", "## postgres verbs", "exec"},
		"skill lint":     {"# tmvp skill lint", "structure only"},
		"prod api call":  {"http call", "read-write", "tmvp write prod api call"},
	}
	for line, wants := range cases {
		var out bytes.Buffer
		stdout = &out
		root := newRoot()
		args, ok := stripLLMHelp(append(strings.Fields(line), "--llmhelp"))
		if !ok {
			t.Fatal("not stripped")
		}
		llmHelp(root, args)
		for _, w := range wants {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%q: missing %q in\n%s", line, w, out.String())
			}
		}
	}
	stdout = os.Stdout
}
