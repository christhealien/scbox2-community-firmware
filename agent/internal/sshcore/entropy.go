package sshcore

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// WaitEntropy: before a key that lasts is made, wait (at most wait) until devRandom is readable, then mix 32
// bytes from it into devURandom (the panel's access.go explains why: the SC Box II's Linux 3.10 has no
// getrandom, and Go then reads /dev/urandom without any wait, even before the pool is seeded).
func WaitEntropy(devRandom, devURandom string, wait time.Duration, logf func(string, ...any)) {
	fd, err := syscall.Open(devRandom, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return // no /dev/random: nothing to wait on (crypto/rand reports its own trouble)
	}
	defer syscall.Close(fd)
	start := time.Now()
	for {
		ok, err := fdReadable(fd, time.Second)
		if ok {
			break
		}
		if err != nil && err != syscall.EINTR {
			return
		}
		if time.Since(start) > wait {
			logf("random: the kernel's pool still isn't seeded after %s: making keys anyway", wait)
			return
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		logf("random: waited %.1f s for the kernel's random pool before making keys", d.Seconds())
	}
	entropyMix(fd, devRandom, devURandom, start.Add(wait), logf)
}

// fdReadable: select(2) on fd for up to d.
func fdReadable(fd int, d time.Duration) (bool, error) {
	var set syscall.FdSet
	w := 8 * int(unsafe.Sizeof(set.Bits[0]))
	set.Bits[fd/w] |= 1 << (uint(fd) % uint(w))
	tv := syscall.NsecToTimeval(int64(d))
	n, err := syscall.Select(fd+1, &set, nil, nil, &tv)
	return err == nil && n > 0, err
}

// entropyMix: 32 bytes from devRandom (fd, non-blocking: waited for by select until deadline) written to
// devURandom, so urandom's pool holds them before a key is made.
func entropyMix(fd int, devRandom, devURandom string, deadline time.Time, logf func(string, ...any)) {
	buf := make([]byte, 32)
	n := 0
	for n < len(buf) {
		m, err := syscall.Read(fd, buf[n:])
		if m > 0 {
			n += m
			continue
		}
		if err != nil && err != syscall.EAGAIN && err != syscall.EINTR || err == nil && m == 0 {
			logf("random: couldn't read %s to reseed %s (%v)", devRandom, devURandom, err)
			return
		}
		left := time.Until(deadline)
		if left <= 0 {
			logf("random: %s gave %d of 32 bytes in time: %s not reseeded before making keys", devRandom, n, devURandom)
			return
		}
		if left > time.Second {
			left = time.Second
		}
		if _, err := fdReadable(fd, left); err != nil && err != syscall.EINTR {
			return
		}
	}
	f, err := os.OpenFile(devURandom, os.O_WRONLY, 0)
	if err != nil {
		logf("random: %s: %v", devURandom, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(buf); err != nil {
		logf("random: writing %s: %v", devURandom, err)
		return
	}
	logf("random: mixed 32 bytes from %s into %s before making keys", devRandom, devURandom)
}
