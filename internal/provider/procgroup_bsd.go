//go:build unix && !linux

package provider

import "syscall"

// setParentDeathSignal does nothing here: macOS and the BSDs have no
// equivalent, so a daemon that crashes leaves its CLI agent running.
func setParentDeathSignal(*syscall.SysProcAttr) {}
