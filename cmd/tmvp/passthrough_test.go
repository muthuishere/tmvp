package main

import (
	"reflect"
	"testing"
)

func TestPassUnknownFlags(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"prod", "box", "docker", "logs", "api", "--tail", "50"},
			[]string{"--", "prod", "box", "docker", "logs", "api", "--tail", "50"}},
		{[]string{"--config", "t.yaml", "box", "docker", "ps", "-a", "--json"},
			[]string{"--config", "t.yaml", "--json", "--", "box", "docker", "ps", "-a"}},
		{[]string{"write", "prod", "box", "run", "x", "--reason", "fix it", "--force", "--zz=1", "-Fcontainer=api"},
			[]string{"write", "--reason", "fix it", "--force", "-Fcontainer=api", "--", "prod", "box", "run", "x", "--zz=1"}},
		// nothing unknown: untouched
		{[]string{"prod", "db", "select 1", "--limit", "5"}, []string{"prod", "db", "select 1", "--limit", "5"}},
		// explicit -- from the user: untouched
		{[]string{"write", "k", "exec", "pod", "--", "ls", "-la"}, []string{"write", "k", "exec", "pod", "--", "ls", "-la"}},
		// other commands: untouched
		{[]string{"config", "--bogus"}, []string{"config", "--bogus"}},
	}
	for _, c := range cases {
		if got := passUnknownFlags(newRoot(), c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
