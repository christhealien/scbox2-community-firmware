package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Started the way the boot script starts the panel (SIGINT ignored on entry), a program the panel starts
// must still stop on Ctrl+C (SIGINT).
func TestChildrenGetCtrlC(t *testing.T) {
	if os.Getenv(brandEnv("SIGHELPER")) == "1" {
		c := exec.Command("sleep", "20")
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		syscall.Kill(c.Process.Pid, syscall.SIGINT)
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			c.Process.Kill()
			t.Fatal("the child ignored SIGINT")
		}
		return
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep")
	}
	cmd := exec.Command("sh", "-c", "trap '' INT QUIT; exec \"$0\" -test.run=TestChildrenGetCtrlC -test.count=1", os.Args[0])
	cmd.Env = append(os.Environ(), brandEnv("SIGHELPER")+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
}
