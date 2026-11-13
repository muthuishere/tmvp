// Package clitest installs fake CLIs for connector tests.
package clitest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Fake writes a POSIX shell script named name into a temp dir and puts that
// dir first on PATH. Skips on Windows.
func Fake(t testing.TB, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLIs are shell scripts")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}
