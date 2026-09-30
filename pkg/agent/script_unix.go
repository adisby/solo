//go:build !windows

package agent

import (
	"os"
	"os/exec"
)

// resolveScriptCommand adapts a launcher script into the command that runs it.
//
// POSIX kernels honour a shebang, so an executable script is already a runnable
// program and the script itself stays the command. A script without the execute
// bit is not: that is the normal state of a JavaScript entry point inside an npm
// package, and of a .js file copied from a Windows checkout. Those fall back to
// node, which is what Windows has to do unconditionally.
func resolveScriptCommand(scriptPath string) (string, []string) {
	if isDirectlyExecutable(scriptPath) {
		return scriptPath, nil
	}
	if isJavaScriptEntryPoint(scriptPath) {
		if node, err := exec.LookPath("node"); err == nil {
			return node, []string{scriptPath}
		}
	}
	// Leave the caller's error reporting intact: an unadapted path stays
	// unresolved and detection reports the runtime as unavailable.
	return scriptPath, nil
}

// isDirectlyExecutable reports whether the kernel can start path itself.
func isDirectlyExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
