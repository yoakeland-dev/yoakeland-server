package provider

import "os/exec"

// DetectCLI reports whether the given CLI's binary is on PATH, so a
// frontend can show subscription-mode providers as available/unavailable
// per machine before the user tries to register one.
func DetectCLI(kind CLIKind) bool {
	_, err := exec.LookPath(string(kind))
	return err == nil
}
