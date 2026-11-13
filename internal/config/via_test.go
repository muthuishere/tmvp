package config

import (
	"strings"
	"testing"
)

const containerSample = `
version: 1
product: demo
secrets:
  ssh_key: "$(echo FAKE_SECRET_123)"
envs:
  prod:
    systems:
      box: { type: ssh, host: 10.0.0.5, user: deploy, key: "${secrets.ssh_key}" }
      pg:  { type: postgres, via: [box, {container: pg}], read: { dsn: "${secrets.ssh_key}" } }
      pg2: { type: postgres, via: [box, {container: pg, network: backend, sudo: true}], read: { dsn: "${secrets.ssh_key}" } }
`

func TestContainerHopParseAndChain(t *testing.T) {
	c := mustParse(t, containerSample)
	if got := c.System("prod", "pg2").Via; len(got) != 2 || got[1] != "container:pg@backend+sudo" {
		t.Fatalf("via %v", got)
	}
	if is := errorsOf(c.Validate(ValidateOptions{})); len(is) != 0 {
		t.Fatalf("issues %v", is)
	}
	ch, err := c.ViaChain("prod", "pg2")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 2 || ch[0].Name != "box" || ch[1].Type != ContainerHopType ||
		ch[1].Fields["container"] != "pg" || ch[1].Fields["network"] != "backend" || ch[1].Fields["sudo"] != true {
		t.Fatalf("chain %+v", ch[1])
	}
	h, ok := ParseContainerHop("container:pg@net+sudo")
	if !ok || h != (ContainerHop{Name: "pg", Network: "net", Sudo: true}) || h.Token() != "container:pg@net+sudo" {
		t.Fatalf("parse %+v", h)
	}
	if _, ok := ParseContainerHop("box"); ok {
		t.Fatal("plain name parsed as container")
	}
}

func TestContainerHopValidate(t *testing.T) {
	bad := `
version: 1
product: demo
envs:
  prod:
    systems:
      box: { type: ssh, host: 10.0.0.5, user: deploy, insecure: true }
      a: { type: postgres, via: [{container: pg}] }
      b: { type: postgres, via: [box, {container: "pg;rm -rf /"}] }
      c: { type: postgres, via: [box, {container: pg}, box] }
`
	c := mustParse(t, bad)
	var msgs []string
	for _, i := range errorsOf(c.Validate(ValidateOptions{})) {
		msgs = append(msgs, i.String())
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"prod.systems.a.via", "must directly follow an ssh hop", "must match", "must be the last hop"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if _, err := c.ViaChain("prod", "b"); err == nil {
		t.Fatal("ViaChain accepted a bad container name")
	}
	if _, err := Parse([]byte("version: 1\nproduct: d\nenvs:\n  p:\n    systems:\n      x: { type: postgres, via: [{image: pg}] }\n")); err == nil {
		t.Fatal("unknown key in via mapping accepted")
	}
}
