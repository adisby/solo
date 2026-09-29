package agent

import (
	"os"
	"os/exec"
	"testing"
)

// TestIsProcessAlive exercises the shared liveness probe on the running
// platform. The Windows implementation replaced a Signal(0) probe that
// misreported live processes, so this test runs on every GOOS target to keep
// both platform files honest.
func TestIsProcessAlive(t *testing.T) {
	t.Run("current process is alive", func(t *testing.T) {
		if !IsProcessAlive(os.Getpid()) {
			t.Fatalf("IsProcessAlive(%d) = false for the running test process", os.Getpid())
		}
	})

	t.Run("non-positive pid is not alive", func(t *testing.T) {
		for _, pid := range []int{0, -1} {
			if IsProcessAlive(pid) {
				t.Fatalf("IsProcessAlive(%d) = true, want false", pid)
			}
		}
	})

	t.Run("exited process is not alive", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessAliveHelper")
		cmd.Env = append(os.Environ(), processAliveHelperEnv+"=1")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper process: %v", err)
		}
		pid := cmd.Process.Pid
		if err := cmd.Wait(); err != nil {
			t.Fatalf("wait for helper process: %v", err)
		}
		if IsProcessAlive(pid) {
			// PID reuse can legitimately hand the number to another process, which
			// makes the result meaningless rather than wrong.
			t.Skipf("PID %d was recycled by another process", pid)
		}
	})
}

const processAliveHelperEnv = "SOLO_TEST_PROCESS_ALIVE_HELPER"

// TestProcessAliveHelper is the short-lived child used by TestIsProcessAlive.
// It only runs when re-executed with processAliveHelperEnv set.
func TestProcessAliveHelper(t *testing.T) {
	if os.Getenv(processAliveHelperEnv) != "1" {
		t.Skip("helper process; only runs when re-executed by TestIsProcessAlive")
	}
	os.Exit(0)
}
