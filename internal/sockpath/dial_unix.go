//go:build !windows

package sockpath

import "net"

// Dial connects to a yoakeland local socket at path.
func Dial(path string) (net.Conn, error) { return net.Dial("unix", path) }
