//go:build unix

package limits

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCommandCancellationKillsDescendants(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runCommand(ctx, "sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "sh", pidPath)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	var child int
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if child > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("child did not start")
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled command returned success")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("cancellation hung on descendant pipes")
	}
	// Linux can briefly leave a killed child as a zombie until PID 1 reaps it.
	for time.Now().Before(deadline) {
		if syscall.Kill(child, 0) == syscall.ESRCH {
			return
		}
		data, _ := os.ReadFile(fmtProcStat(child))
		if end := strings.LastIndex(string(data), ")"); end >= 0 && strings.HasPrefix(string(data)[end+1:], " Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("descendant survived command cancellation")
}

func fmtProcStat(pid int) string { return "/proc/" + strconv.Itoa(pid) + "/stat" }
