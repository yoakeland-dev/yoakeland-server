//go:build !unix

package main

import "os"

// ponytail: no server lock off unix; add LockFileEx if Windows ever runs stable and early together.
func lockServer() (*os.File, error) { return nil, nil }
