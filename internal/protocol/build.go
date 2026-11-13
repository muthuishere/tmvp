package protocol

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
)

var (
	buildOnce sync.Once
	buildID   string
)

// SelfBuild identifies the running executable's build, so a client can tell
// that the server was started from a different (older or newer) binary even
// when both report the same version ("dev", or a reinstall of one tag). It is
// the Go build ID the linker writes near the start of every Go binary; when
// that cannot be read it falls back to the executable's size and mtime.
func SelfBuild() string {
	buildOnce.Do(func() { buildID = readBuild() })
	return buildID
}

func readBuild() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 256<<10)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	marker := []byte("\xff Go build ID: \"")
	if i := bytes.Index(head, marker); i >= 0 {
		rest := head[i+len(marker):]
		if j := bytes.IndexByte(rest, '"'); j > 0 && j < 200 {
			return string(rest[:j])
		}
	}
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("size%d-mtime%d", st.Size(), st.ModTime().UnixNano())
}
