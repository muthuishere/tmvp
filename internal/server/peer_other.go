//go:build !linux && !darwin

package server

import "net"

// checkPeer: Windows AF_UNIX has no SO_PEERCRED equivalent. Isolation comes
// from the protected owner+SYSTEM DACL on the run dir and socket file
// (secureDir/secureFile): connect() requires write access to the socket
// file, so other non-admin users cannot reach the server at all.
func checkPeer(net.Conn) error { return nil }
