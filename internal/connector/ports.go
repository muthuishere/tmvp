package connector

import (
	"hash/fnv"
	"strconv"
)

// Local listener port convention, shared by `tmvp <env> <portforward> open`
// and `tmvp export pg-service`, so plain psql/clients line up with tmvp's
// forward without anyone passing -F port=N.
const (
	LocalPortBase = 15432
	LocalPortSpan = 1000
)

// LocalPort is the default 127.0.0.1 port for product/env/system:
// LocalPortBase + fnv32(product/env/system) % LocalPortSpan. Stable across
// runs and machines; a system's `local_port:` field overrides it.
func LocalPort(product, env, system string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(product + "/" + env + "/" + system))
	return LocalPortBase + int(h.Sum32()%LocalPortSpan)
}

// LocalPortString is LocalPort as a string.
func LocalPortString(product, env, system string) string {
	return strconv.Itoa(LocalPort(product, env, system))
}
