//go:build !linux

package secret

func materializeMem(string, []byte) (string, func(), bool) { return "", nil, false }
