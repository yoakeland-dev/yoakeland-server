//go:build !unix

package main

import "os"

// ponytail: no project lock off unix; add LockFileEx if Windows ever runs stable and early together.
func lockProject(string) (*os.File, error) { return nil, nil }
