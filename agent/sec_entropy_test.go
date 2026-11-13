package main

// L12: before a key that lasts is made, the panel waits for the kernel's random pool (/dev/random readable):
// Go 1.24's crypto/rand reads /dev/urandom without any wait on a kernel without getrandom (Linux 3.10).

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSecEntropyWait(t *testing.T) {
	od, ou, ow := devRandom, devURandom, entropyWait
	t.Cleanup(func() { devRandom, devURandom, entropyWait, entropyOnce = od, ou, ow, sync.Once{} })
	devURandom = filepath.Join(t.TempDir(), "urandom") // (the mix after the wait: never the real one here)
	os.WriteFile(devURandom, nil, 0o600)
	// a pool that never becomes ready (a FIFO nobody writes to): waits, then goes on (bounded)
	fifo := filepath.Join(t.TempDir(), "random")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	w, err := os.OpenFile(fifo, os.O_RDWR, 0) // keeps a writer, so reads would block (not EOF)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	devRandom, entropyWait, entropyOnce = fifo, 1500*time.Millisecond, sync.Once{}
	t0 := time.Now()
	entropyReady()
	if d := time.Since(t0); d < time.Second || d > 5*time.Second {
		t.Fatalf("an unready pool: waited %v, want about the limit", d)
	}
	// ready (32 bytes to read, mixed into urandom): no wait
	w.Write([]byte("0123456789abcdef0123456789abcdef"))
	entropyOnce = sync.Once{}
	t0 = time.Now()
	entropyReady()
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("a ready pool: waited %v", d)
	}
	// the real /dev/random of the machine running the tests (seeded): no wait
	devRandom, entropyOnce = od, sync.Once{}
	t0 = time.Now()
	entropyReady()
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("/dev/random: waited %v", d)
	}
}
