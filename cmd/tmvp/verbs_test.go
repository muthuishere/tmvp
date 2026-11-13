package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestVerbsCommand(t *testing.T) {
	var out bytes.Buffer
	stdout = &out
	defer func() { stdout = os.Stdout }()
	root := newRoot()
	root.SetArgs([]string{"verbs", "web", "ssh", "aws"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"web          click", "read-write", "ssh          script", "@item", "aws          call", "default"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, " act ") {
		t.Error("web act verb should be gone")
	}
	root = newRoot()
	root.SetArgs([]string{"verbs", "nope"})
	if err := root.Execute(); err == nil {
		t.Fatal("unknown type must fail")
	}
}
