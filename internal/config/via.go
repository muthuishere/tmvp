package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ContainerHopType is the System.Type of a synthetic `{container: name}` via hop.
const ContainerHopType = "container"

// containerName is the docker container/network name rule (also blocks shell
// metacharacters: names are passed to `docker` over ssh).
var containerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidContainerName reports whether s is a safe docker container/network name.
func ValidContainerName(s string) bool { return containerName.MatchString(s) }

// ContainerHop is a parsed `{container: pg, network: x, sudo: true}` via entry.
// It is stored in System.Via as its canonical token "container:pg[@x][+sudo]".
type ContainerHop struct {
	Name    string
	Network string
	Sudo    bool
}

// Token is the canonical via string (shown in chains: "box>container:pg@backend").
func (h ContainerHop) Token() string {
	s := "container:" + h.Name
	if h.Network != "" {
		s += "@" + h.Network
	}
	if h.Sudo {
		s += "+sudo"
	}
	return s
}

// ParseContainerHop parses a via token "container:<name>[@<network>][+sudo]".
// ok is false for ordinary system names.
func ParseContainerHop(v string) (h ContainerHop, ok bool) {
	rest, ok := strings.CutPrefix(v, "container:")
	if !ok {
		return h, false
	}
	rest, h.Sudo = strings.CutSuffix(rest, "+sudo")
	h.Name, h.Network, _ = strings.Cut(rest, "@")
	return h, true
}

// Validate checks the names.
func (h ContainerHop) Validate() error {
	if !ValidContainerName(h.Name) {
		return fmt.Errorf("container %q: must match ^[A-Za-z0-9][A-Za-z0-9_.-]*$", h.Name)
	}
	if h.Network != "" && !ValidContainerName(h.Network) {
		return fmt.Errorf("network %q: must match ^[A-Za-z0-9][A-Za-z0-9_.-]*$", h.Network)
	}
	return nil
}

// System returns the synthetic hop system the tunneler opens (type container,
// fields container/network/sudo).
func (h ContainerHop) System(env string) *System {
	f := map[string]any{"container": h.Name}
	if h.Network != "" {
		f["network"] = h.Network
	}
	if h.Sudo {
		f["sudo"] = true
	}
	return &System{Name: h.Token(), Env: env, Type: ContainerHopType, Labels: map[string]string{}, Fields: f}
}

// viaEntry turns one YAML via list item (a system name or a {container: ...}
// mapping) into its via string.
func viaEntry(x any) (string, error) {
	m, ok := x.(map[string]any)
	if !ok {
		return fmt.Sprint(x), nil
	}
	var h ContainerHop
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		switch k {
		case "container":
			h.Name = fmt.Sprint(v)
		case "network":
			h.Network = fmt.Sprint(v)
		case "sudo":
			h.Sudo = v == true || v == "true"
		default:
			return "", fmt.Errorf("unknown key %q (want container, network, sudo)", k)
		}
	}
	if h.Name == "" {
		return "", fmt.Errorf("mapping entry needs container: <name>")
	}
	return h.Token(), nil
}
