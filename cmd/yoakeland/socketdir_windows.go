package main

import (
	"fmt"
	"net"
	"strings"
	"time"
	"yoakeland/internal/sockpath"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// setupLocalListener opens the always-on local listener on Windows: a named
// pipe whose security descriptor makes this user its owner and grants access
// to this user's SID alone — the equivalent of the unix socket's 0600, and
// just as much the whole auth boundary (connections get "unix" semantics, no
// token). go-winio already rejects remote (SMB) clients and creates the first
// instance with FILE_CREATE, so a name someone else already holds fails here
// rather than being shared. ownDir is meaningless for a pipe.
//
// A path without the \\.\pipe\ prefix (e.g. -listen unix:foo) is taken as a
// pipe name.
func setupLocalListener(path string, _ bool) (net.Listener, error) {
	if !strings.HasPrefix(path, sockpath.PipePrefix) {
		path = sockpath.PipePrefix + path
	}
	timeout := time.Second
	if c, err := winio.DialPipe(path, &timeout); err == nil {
		c.Close()
		return nil, fmt.Errorf("another yoakeland is already listening on %s", path)
	}
	sddl, err := ownerOnlySDDL()
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		return nil, fmt.Errorf("creating named pipe %s (already owned by another process?): %w", path, err)
	}
	logInfoDetail(
		fmt.Sprintf("local pipe listening at %s", path),
		fmt.Sprintf("yoakeland local named pipe listening at %s (owner-only ACL; no auth token needed, the ACL is the auth boundary)", path),
	)
	return ln, nil
}

// ownerOnlySDDL: owner = this user, protected DACL (no inherited ACEs) with a
// single generic-all ACE for this user.
func ownerOnlySDDL() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("looking up current user SID: %w", err)
	}
	sid := u.User.Sid.String()
	return "O:" + sid + "D:P(A;;GA;;;" + sid + ")", nil
}
