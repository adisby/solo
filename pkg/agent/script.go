package agent

import (
	"path/filepath"
	"strings"
)

// isJavaScriptEntryPoint reports whether path names the JavaScript launcher of a
// CLI that is normally started through an interpreter rather than by its own
// name. DSH is packaged this way: DSH_BIN points at bin.js.
func isJavaScriptEntryPoint(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".js")
}

// adaptScriptCommand offers a configured binary path to an adapter's script
// resolver and reports whether the resolver produced a different command.
//
// It exists for the case exec.LookPath rejects: a launcher script that is not a
// runnable program on its own. A .js entry point has no execute bit on Windows,
// and a POSIX checkout may lack the bit too, so detection has to be allowed to
// fall back to the interpreter instead of reporting the runtime as unavailable.
func adaptScriptCommand(resolver ScriptCommandResolver, candidate string) (string, []string, bool) {
	if resolver == nil {
		return "", nil, false
	}
	execPath, leadingArgs := resolver(candidate)
	if execPath == "" || execPath == candidate {
		return "", nil, false
	}
	return execPath, leadingArgs, true
}
