package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/rand"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
)

func TestSplitArgsAndNeedsShell(t *testing.T) {
	got, err := SplitArgs(`sec get "A B" 'c d' e\ f --print`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sec", "get", "A B", "c d", "e f", "--print"}
	if runtime.GOOS == "windows" {
		want = []string{"sec", "get", "A B", "c d", `e\`, "f", "--print"}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
	for cmd, want := range map[string]bool{
		"sec get X --print":         false,
		"op read op://a/b":          false,
		"vault kv get x | jq -r .v": true,
		"cat f > out":               true,
		"echo $HOME":                true,
		`echo "a|b"`:                false,
		"gcloud secrets versions access latest --secret=x": false,
	} {
		if NeedsShell(cmd) != want {
			t.Errorf("NeedsShell(%q) != %v", cmd, want)
		}
	}
}

func TestStoreLazySingleFlight(t *testing.T) {
	var n atomic.Int32
	r := RunnerFunc(func(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) {
		n.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []byte("FAKE_SECRET_123\n"), nil
	})
	red := NewRedactor()
	s := NewStore("demo", r, red)
	d := Def{Key: "/a", Name: "secrets.a", Command: "x", Fingerprint: "1"}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Get(context.Background(), d)
			if err != nil || v != "FAKE_SECRET_123" {
				t.Errorf("got %q %v", v, err)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("ran %d times", n.Load())
	}
	if red.Redact("x FAKE_SECRET_123 y") != "x "+Mask+" y" {
		t.Fatal("value not registered with redactor")
	}
	// fingerprint change → rerun
	d.Fingerprint = "2"
	_, _ = s.Get(context.Background(), d)
	if n.Load() != 2 {
		t.Fatal("changed definition must re-run")
	}
	if dropped := s.Retain(nil); len(dropped) != 1 {
		t.Fatalf("retain dropped %v", dropped)
	}
}

func TestPrefetchParallel(t *testing.T) {
	r := RunnerFunc(func(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) {
		time.Sleep(100 * time.Millisecond)
		if cmd == "fail" {
			return nil, errors.New("access denied")
		}
		return []byte("FAKE_" + cmd), nil
	})
	s := NewStore("demo", r, nil)
	start := time.Now()
	res := s.Prefetch(context.Background(), []Def{
		{Key: "/a", Name: "a", Command: "a"}, {Key: "/b", Name: "b", Command: "b"}, {Key: "/c", Name: "c", Command: "fail"},
	})
	if el := time.Since(start); el > 250*time.Millisecond {
		t.Fatalf("not parallel: %v", el)
	}
	if res["/a"] != nil || res["/b"] != nil {
		t.Fatal(res)
	}
	if errs.CodeOf(res["/c"]) != errs.SecretMissing {
		t.Fatalf("want secret_missing, got %v", res["/c"])
	}
}

func TestExpiryJSON(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var n atomic.Int32
	r := RunnerFunc(func(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) {
		n.Add(1)
		return []byte(`{"value":"FAKE_TOKEN_` + string(rune('0'+n.Load())) + `","expires_at":"2026-01-01T00:10:00Z"}`), nil
	})
	s := NewStore("demo", r, nil)
	s.now = func() time.Time { return now }
	d := Def{Key: "/t", Name: "t", Command: "x"}
	v, _ := s.Get(context.Background(), d)
	if v != "FAKE_TOKEN_1" {
		t.Fatal(v)
	}
	now = now.Add(5 * time.Minute)
	v, _ = s.Get(context.Background(), d)
	if v != "FAKE_TOKEN_1" {
		t.Fatal("refetched too early")
	}
	now = now.Add(4*time.Minute + 30*time.Second) // within refresh margin
	v, _ = s.Get(context.Background(), d)
	if v != "FAKE_TOKEN_2" {
		t.Fatalf("not refreshed before expiry: %s", v)
	}
}

func TestInvalidate(t *testing.T) {
	val := "FAKE_OLD_VALUE"
	r := RunnerFunc(func(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) { return []byte(val), nil })
	s := NewStore("demo", r, nil)
	d := Def{Key: "/a", Name: "a", Command: "x"}
	_, _ = s.Get(context.Background(), d)
	if err := s.Invalidate(context.Background(), d); errs.CodeOf(err) != errs.SecretInvalid {
		t.Fatalf("same value must be secret_invalid, got %v", err)
	}
	val = "FAKE_NEW_VALUE"
	if err := s.Invalidate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get(context.Background(), d); v != "FAKE_NEW_VALUE" {
		t.Fatal(v)
	}
	s.Clear()
	if len(s.Status()) != 0 {
		t.Fatal("clear")
	}
}

func TestExecRunnerRealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	r := ExecRunner{Timeout: 5 * time.Second}
	out, err := r.Run(context.Background(), "printf FAKE_SECRET_123", false, "")
	if err != nil || string(out) != "FAKE_SECRET_123" {
		t.Fatalf("%q %v", out, err)
	}
	out, err = r.Run(context.Background(), "echo a b | tr a-z A-Z", false, "")
	if err != nil || strings.TrimSpace(string(out)) != "A B" {
		t.Fatalf("shell: %q %v", out, err)
	}
	// $file(cmd): command writes ${out}; file is deleted afterwards
	var outPath string
	out, err = r.Run(context.Background(), `sh -c 'printf FAKE_FILE_CONTENT > "$0"; echo "$0"' ${out}`, true, "")
	if err != nil || string(out) != "FAKE_FILE_CONTENT" {
		t.Fatalf("$file: %q %v", out, err)
	}
	_ = outPath
	// never interactive: stdin is null → `cat` returns immediately
	start := time.Now()
	if _, err := r.Run(context.Background(), "cat", false, ""); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("stdin not null")
	}
	// timeout
	r.Timeout = 200 * time.Millisecond
	if _, err := r.Run(context.Background(), "sleep 5", false, ""); err == nil {
		t.Fatal("timeout expected")
	}
}

func TestFileSecretTempIsDeleted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	rec := dir + "/path"
	r := ExecRunner{}
	_, err := r.Run(context.Background(), `sh -c 'printf FAKE > "$0"; printf %s "$0" > `+rec+`' ${out}`, true, "")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := os.ReadFile(rec)
	if _, err := os.Stat(string(p)); !os.IsNotExist(err) {
		t.Fatalf("temp %s still exists", p)
	}
}

func TestMaterialize(t *testing.T) {
	p, cleanup, err := Materialize("kube", []byte("FAKE_KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "FAKE_KUBECONFIG" {
		t.Fatalf("%q %v", b, err)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("perm %v", st.Mode())
		}
	}
	cleanup()
	if runtime.GOOS != "linux" {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("not deleted")
		}
	}
}

// TestCanaryNoForm is the S1 canary: the value and its base64/url/hex/JSON
// forms never pass the redacting writer, even split across random writes.
func TestCanaryNoForm(t *testing.T) {
	canary := "FAKE_CANARY_s3cr3t/+=&? \"x\"\nline2-is-long-enough"
	red := NewRedactor()
	red.Add(canary)
	forms := []string{
		canary,
		base64.StdEncoding.EncodeToString([]byte(canary)),
		base64.RawURLEncoding.EncodeToString([]byte(canary)),
		hex.EncodeToString([]byte(canary)),
		strings.ToUpper(hex.EncodeToString([]byte(canary))),
		url.QueryEscape(canary),
		url.PathEscape(canary),
		`FAKE_CANARY_s3cr3t/+=&? \"x\"\nline2-is-long-enough`, // JSON-escaped
	}
	var input strings.Builder
	for i, f := range forms {
		input.WriteString("prefix-")
		input.WriteString(f)
		input.WriteString(" middle ")
		if i%2 == 0 {
			input.WriteString("\n")
		}
	}
	src := []byte(input.String())
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 200; trial++ {
		var out bytes.Buffer
		w := red.Writer(&out)
		for i := 0; i < len(src); {
			n := 1 + rng.Intn(17)
			if i+n > len(src) {
				n = len(src) - i
			}
			if _, err := w.Write(src[i : i+n]); err != nil {
				t.Fatal(err)
			}
			i += n
		}
		_ = w.Flush()
		got := out.String()
		for _, f := range append(forms, "s3cr3t", "line2-is-long-enough") {
			if strings.Contains(got, f) {
				t.Fatalf("trial %d leaked form %q in %q", trial, f, got)
			}
		}
		if !strings.Contains(got, "prefix-") || !strings.Contains(got, " middle ") {
			t.Fatal("non-secret text lost")
		}
	}
}

func TestRedactorShortValuesIgnored(t *testing.T) {
	red := NewRedactor()
	red.Add("ab")
	if red.Redact("ab cd") != "ab cd" {
		t.Fatal("too-short values must not be redacted")
	}
}
