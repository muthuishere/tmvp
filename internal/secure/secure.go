// Package secure makes files and directories owner-only on every OS:
// 0700/0600 on Unix, a protected DACL granting only the current user SID
// and SYSTEM on Windows (inherited by everything created inside a secured
// dir). Secure a directory BEFORE writing secrets into it, so there is no
// window where a secret is readable by another user.
package secure

import "os"

// Dir makes an existing directory owner-only.
func Dir(path string) error {
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return applyOwnerOnly(path)
}

// File makes an existing file owner-only.
func File(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return applyOwnerOnly(path)
}

// MkdirAll creates path (0700) and makes the leaf owner-only.
func MkdirAll(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return Dir(path)
}

// MkdirTemp is os.MkdirTemp followed by Dir; the dir is removed on failure.
func MkdirTemp(base, pattern string) (string, error) {
	d, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return "", err
	}
	if err := Dir(d); err != nil {
		os.RemoveAll(d)
		return "", err
	}
	return d, nil
}

// WriteFile writes data to a new/truncated owner-only file.
func WriteFile(path string, data []byte) error {
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	if err := File(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
