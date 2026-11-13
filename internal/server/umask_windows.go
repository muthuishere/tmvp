//go:build windows

package server

func withPrivateUmask(f func()) { f() }
