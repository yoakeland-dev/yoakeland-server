package sockpath

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Dial connects to a yoakeland named pipe at path, then refuses it unless
// the pipe is owned by this user. Pipe names are global, so another local
// user could create ours first; the owner check is what stops a client from
// handing prompts and credentials to that impostor. (yoakeland sets itself
// as owner explicitly, and setting someone else as owner needs privileges a
// normal user doesn't have.)
func Dial(path string) (net.Conn, error) {
	c, err := winio.DialPipe(path, nil)
	if err != nil {
		return nil, err
	}
	if err := checkPipeOwner(c); err != nil {
		c.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func checkPipeOwner(c net.Conn) error {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("can't inspect pipe handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading pipe owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("reading pipe owner: %w", err)
	}
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !owner.Equals(me.User.Sid) {
		return fmt.Errorf("pipe is owned by %s, not this user; refusing to connect", owner)
	}
	return nil
}
