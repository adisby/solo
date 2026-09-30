package agent

import (
	"os/exec"
)

// resolveScriptCommand adapts a launcher script into the command that runs it.
//
// Windows has no shebang support, so a JavaScript launcher such as DSH's bin.js
// can never be started by path: the interpreter becomes the command and the
// script its first argument. Non-JavaScript paths are returned untouched.
func resolveScriptCommand(scriptPath string) (string, []string) {
	if !isJavaScriptEntryPoint(scriptPath) {
		return scriptPath, nil
	}
	node, err := exec.LookPath("node")
	if err != nil {
		// Leave the caller's error reporting intact: the path stays unresolved
		// and detection reports the runtime as unavailable.
		return scriptPath, nil
	}
	return node, []string{scriptPath}
}
