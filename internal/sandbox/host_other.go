//go:build !linux && !darwin

package sandbox

import (
	"context"
	"errors"
)

const ModeHost SandboxMode = "host"

// ErrHostUnsupported: this platform has no unconfined pty path; callers use SpawnPTY.
var ErrHostUnsupported = errors.New("unconfined terminals aren't supported on this platform")

func SpawnHostPTY(ctx context.Context, argv []string, extraEnv []string, dir string, cols, rows int) (PTYSession, error) {
	return nil, ErrHostUnsupported
}
