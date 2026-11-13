// Package sshcore holds the SSH server pieces shared by the panel's built-in SSH (ssh.go, the SC Lite) and the
// SC Box II's separate SSH server (cmd/scbox2-sshd): the port rules, the host key, who counts as local, the
// session handling (shell, command, SFTP, the forced first password change) and the kernel random-pool wait.
// It must not depend on the panel (package main): the SC Box II's SSH keeps working when the panel binary is
// damaged.
package sshcore

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	DefaultPort  = 22
	MaxConns     = 8                // open SSH connections at once
	MaxHandshake = 3                // sign-ins being checked at once (each check costs ~PBKDF2)
	FailLimit    = 5                // wrong passwords from one address ...
	FailWindow   = 10 * time.Minute // ... within this long block it for the same time
	PreAuthPerIP = 2                // connections from one address not signed in yet (idle ones can't take every slot)
	KeepEvery    = 60 * time.Second // keepalive request to every signed-in client
	KeepWait     = 60 * time.Second // no answer in this long: the client is gone, the connection is closed
	PanelPort    = 8080             // the panel's default port
	MinerAPIPort = 4028             // the mining program's own API port
)

// HostKeyName: the host key's file name, next to the panel's login file.
const HostKeyName = "ssh_host_ed25519_key"

// RefuseOutsideLine: what a peer from outside the local network is sent before the connection is closed (RFC
// 4253 lets a server send lines before its version; ssh -v shows it).
const RefuseOutsideLine = "Refused: SSH on this miner answers only on the local network (Settings -> Network).\r\n"

// PortBad: why p can't be SSH's port ("" = fine), with web the panel's port now: not the panel's ports, not the
// miner's own.
func PortBad(p, web int) string {
	if p < 1 || p > 65535 {
		return "port must be 1–65535"
	}
	switch {
	case p == web || p == PanelPort || p == 80 || p == 443:
		return fmt.Sprintf("port %d is the control panel's: SSH would take it and the panel couldn't open after a reboot", p)
	case p == MinerAPIPort:
		return "port 4028 is the mining program's own"
	case p < 1024 && p != 22:
		return "use 22 or a port from 1024 up"
	}
	return ""
}

// Port: the port SSH uses for a saved port (0 = 22), with web the panel's port: a saved port that would take the
// panel's (or the miner's) can't lock anyone out, 22 is used instead.
func Port(saved, web int) int {
	if saved >= 1 && saved <= 65535 && PortBad(saved, web) == "" {
		return saved
	}
	return DefaultPort
}

// WebPort: the port the panel opens for its saved one (the panel's loadNetConfig rule: 1024-65535, not the
// miner's 4028, not SSH's; else 8080). SSH's own port is then Port(sshSaved, WebPort(...)).
func WebPort(saved, sshSaved int) int {
	if saved >= 1024 && saved <= 65535 && saved != MinerAPIPort && saved != Port(sshSaved, PanelPort) {
		return saved
	}
	return PanelPort
}

// FailKey: the key wrong passwords are counted under. IPv6 addresses count per /64 (one machine can use as
// many addresses in its /64 as it likes).
func FailKey(ip string) string {
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip
}

// HostOf: the address part of a peer address.
func HostOf(a net.Addr) string {
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}

// RemoteIP: a peer's IP (nil when it isn't one).
func RemoteIP(a net.Addr) net.IP {
	if t, ok := a.(*net.TCPAddr); ok {
		return t.IP
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(h)
}

// LocalPeer: loopback, private (RFC 1918, CGNAT, link-local, IPv6 ULA / link-local), or an address inside a
// prefix of one of this miner's own interfaces (addrs: the same LAN: a global IPv6 /64, or an IPv4 LAN that
// uses public addresses, so its owner is never locked out). The panel and SSH aren't for the internet.
func LocalPeer(ip net.IP, addrs func() ([]net.Addr, error)) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64/10 (carrier-grade NAT, Tailscale): a trade-off kept on purpose. Other customers of the same
		// provider share it, but VPN users (Tailscale) would otherwise be locked out.
		if v4[0] == 100 && v4[1]&0xc0 == 64 {
			return true
		}
		as, _ := addrs()
		for _, a := range as { // the miner's own IPv4 subnet (a /8 or narrower: never "everything")
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
				if ones, bits := n.Mask.Size(); bits == 32 && ones >= 8 && n.Contains(v4) {
					return true
				}
			}
		}
		return false
	}
	// a global IPv6 address in the same /64 as one of the miner's own (a DHCPv6 address is a /128 itself)
	as, _ := addrs()
	mask := net.CIDRMask(64, 128)
	for _, a := range as { // (not ::1/128: its /64 would take in ::a.b.c.d, IPv4-compatible addresses)
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() == nil && !n.IP.IsLoopback() && (n.Contains(ip) || n.IP.Mask(mask).Equal(ip.Mask(mask))) {
			return true
		}
	}
	return false
}

// HostKey: the host key at path, made (ed25519, comment) when there is none yet or it can't be parsed. before
// runs first when a key is made (the random-pool wait); logf reports what happened.
func HostKey(path, comment string, before func(), logf func(string, ...any)) (ssh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		if s, err := ssh.ParsePrivateKey(b); err == nil {
			return s, nil
		}
	}
	if before != nil {
		before()
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		logf("ssh: could not save the host key (%v); it will change on restart", err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err == nil {
		logf("ssh: made this miner's host key %s", ssh.FingerprintSHA256(s.PublicKey()))
	}
	return s, err
}

// Fingerprint: the SHA256 fingerprint of the host key saved at path ("" when there is none). Never makes one.
func Fingerprint(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(s.PublicKey())
}
