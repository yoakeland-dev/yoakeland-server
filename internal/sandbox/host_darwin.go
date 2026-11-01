package sandbox

// killTree: no /proc here, so only the shell itself is ended (hostPTYSession.Close).
func killTree(pid int) {}
