//go:build !windows

package secure

// applyOwnerOnly: on Unix the chmod done by the caller is the whole story.
func applyOwnerOnly(string) error { return nil }
