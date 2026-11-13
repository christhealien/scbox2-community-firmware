package main

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// A client that vanishes without closing (Wi-Fi out of range, a laptop asleep): the keepalive is meant to drop
// it, but it waits for the answer with no limit, so the connection (one of only 8 SSH slots) stays until TCP
// itself gives up, about 15 minutes, or forever behind a NAT/proxy that still acks. Here a relay stops passing
// bytes after sign-in, as a vanished client looks from the miner.
func TestL28SSHVanishedClientDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~2 minutes (the keepalive's own pace)")
	}
	addr := sshTestServer(t)
	relay, _ := net.Listen("tcp", "127.0.0.1:0")
	defer relay.Close()
	var frozen atomic.Bool
	go func() {
		for {
			c, err := relay.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", addr)
			if err != nil {
				c.Close()
				continue
			}
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 4096)
				for {
					if frozen.Load() { // gone: nothing passes either way, the sockets stay open
						time.Sleep(time.Hour)
					}
					src.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
					n, err := src.Read(buf)
					if n > 0 {
						dst.Write(buf[:n])
					}
					if err != nil && err != io.EOF {
						if ne, ok := err.(net.Error); ok && ne.Timeout() {
							continue
						}
						return
					}
					if err == io.EOF {
						return
					}
				}
			}
			go pipe(s, c)
			go pipe(c, s)
		}
	}()
	c, err := dialSSH(relay.Addr().String(), defaultUser, defaultPass)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	live := func() int { sshLiveMu.Lock(); defer sshLiveMu.Unlock(); return len(sshLive) }
	if live() != 1 {
		t.Fatalf("want 1 open SSH connection, have %d", live())
	}
	frozen.Store(true)
	deadline := time.Now().Add(140 * time.Second)
	for time.Now().Before(deadline) {
		if live() == 0 {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("a client that vanished 140 s ago still holds an SSH connection slot (%d open)", live())
}
