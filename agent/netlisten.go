package main

// Extra ways in to the panel, besides the main port (8080):
//
//   - port 80 on each LAN address, so typing just the miner's IP opens the panel. The stock web backend
//     (minerd) keeps 127.0.0.1:80, so the agent binds the LAN addresses one by one (a wildcard bind would
//     collide with minerd's loopback socket) and follows DHCP changes.
//   - https: every panel port also accepts TLS (it looks at the first byte of each connection), and 443
//     serves https only. The certificate is made on first start, unique to this miner, and kept with the
//     agent's settings. It's self-signed, so a browser warns once; after that the panel loads over https.

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"sclite-agent/internal/sshcore"
)

var (
	tlsCertOnce sync.Once
	tlsCert     *tls.Certificate
)

func tlsPaths() (string, string) {
	d := filepath.Dir(*authFile)
	return filepath.Join(d, "tls.crt"), filepath.Join(d, "tls.key")
}

// panelCert loads this miner's certificate, making one the first time.
func panelCert() *tls.Certificate {
	tlsCertOnce.Do(func() {
		cp, kp := tlsPaths()
		if c, err := tls.LoadX509KeyPair(cp, kp); err == nil {
			tlsCert = &c
			return
		}
		entropyReady() // a key for the life of the unit: not before the kernel's random pool is seeded (access.go)
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			log.Printf("tls: key: %v", err)
			return
		}
		serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
		host, _ := os.Hostname()
		if host == "" {
			host = brand
		}
		tpl := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{CommonName: brandProduct + " panel (" + host + ")", Organization: []string{brandAgent}},
			// fixed dates: the miner's clock can read 1970 until NTP syncs, and the certificate must
			// already be valid then and for the life of the unit
			NotBefore:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			NotAfter:    time.Date(2049, 12, 31, 0, 0, 0, 0, time.UTC),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames:    []string{host, brand, "localhost"},
			IPAddresses: append(lanIPv4s(), net.IPv4(127, 0, 0, 1)),
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		if err != nil {
			log.Printf("tls: cert: %v", err)
			return
		}
		kb, _ := x509.MarshalECPrivateKey(key)
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
		os.MkdirAll(filepath.Dir(cp), 0o700)
		// FAT can't store 0600; the /mnt/config mount's umask=077 keeps the key private (B8). cfgWrite is
		// crash-safe on FAT and keeps a .bak, unchanged on SC Lite's ext.
		if err := cfgWrite(kp, keyPEM, 0o600); err == nil {
			cfgWrite(cp, certPEM, 0o644)
		}
		c, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			log.Printf("tls: pair: %v", err)
			return
		}
		tlsCert = &c
		log.Printf("tls: made this miner's panel certificate (%s)", cp)
	})
	return tlsCert
}

func lanIPv4s() []net.IP {
	var out []net.IP
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if v4 := n.IP.To4(); v4 != nil && !v4.IsLoopback() && !v4.IsLinkLocalUnicast() {
					out = append(out, v4)
				}
			}
		}
	}
	return out
}

// chanListener hands out connections that a dispatcher already accepted.
type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
	done chan struct{}
	once sync.Once
}

func newChanListener(a net.Addr) *chanListener {
	return &chanListener{ch: make(chan net.Conn, 16), addr: a, done: make(chan struct{})}
}
func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

// give: hand a connection to the server, or close it if the listener has closed (never blocks forever)
func (l *chanListener) give(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		c.Close()
	}
}

// peekConn replays the bytes looked at to tell http from https.
type peekConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func newPanelServer(h http.Handler) *http.Server {
	// WriteTimeout stays 0 so the long-lived log/SSE stream isn't cut; ReadHeaderTimeout bounds
	// slow-header (Slowloris) connections, and IdleTimeout reaps idle keep-alives.
	return &http.Server{Handler: h, WriteTimeout: 0, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 16,
		ErrorLog: log.New(panelErrLog{}, "", 0)} // TLS handshake noise from refused self-signed certs: dropped
}

// panelErrLog: the web server's own messages are dropped (mostly TLS handshakes refused by browsers that don't
// trust the self-signed certificate), except a request handler's panic: net/http recovers it and carries on, so
// its message and stack are the only trace left of it (and of any lock it left held).
type panelErrLog struct{}

func (panelErrLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("http: panic serving")) {
		log.Print(string(p))
	}
	return len(p), nil
}

// servePanel serves h on ln, http and https on the same port (tlsOnly: https only).
func servePanel(ln net.Listener, h http.Handler, tlsOnly bool) error {
	plain, secure := newChanListener(ln.Addr()), newChanListener(ln.Addr())
	defer plain.Close()
	defer secure.Close()
	if c := panelCert(); c != nil {
		cfg := &tls.Config{Certificates: []tls.Certificate{*c}, MinVersion: tls.VersionTLS12}
		go newPanelServer(h).Serve(tls.NewListener(secure, cfg))
	} else if tlsOnly {
		return errors.New("no certificate")
	}
	if !tlsOnly {
		go newPanelServer(h).Serve(plain)
	}
	backoff := 5 * time.Millisecond
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// too many open files and the like: wait and carry on (as net/http does), never stop serving
			time.Sleep(backoff)
			if backoff *= 2; backoff > time.Second {
				backoff = time.Second
			}
			continue
		}
		backoff = 5 * time.Millisecond
		if ip := remoteIPOf(c.RemoteAddr()); ip != nil && !peerAllowed(ip) {
			refuseOutside(c) // from outside the local network: a one-line answer, then closed (access.go)
			continue
		}
		var okc bool
		if c, okc = connAllow(c); !okc {
			continue // (closed) too many from that address
		}
		go func(c net.Conn) {
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReader(c)
			b, err := br.Peek(1)
			c.SetReadDeadline(time.Time{})
			if err != nil {
				c.Close()
				return
			}
			pc := &peekConn{c, br}
			isTLS := b[0] == 0x16 // TLS handshake record
			switch {
			case isTLS && panelCert() != nil:
				secure.give(pc)
			case !isTLS && !tlsOnly:
				plain.give(pc)
			default:
				c.Close()
			}
		}(c)
	}
}

// followLAN keeps a port-80 listener on every LAN IPv4 address, following DHCP changes, until stop is closed
// (Settings -> Network can switch port 80 off: access.go).
//
// Box II port layout (no conflict with the stock backend): the stock web backend (minerd) and the cgminer
// API (intminer) are pinned to loopback by sclite_bind.so at the kit level — 127.0.0.1:80 and 127.0.0.1:4028.
// The panel owns :8080 (or the saved port), :443, :22, and :80 on each LAN address. followLAN binds each LAN
// IPv4 one by one (never a wildcard :80, which would collide with minerd's 127.0.0.1:80), and the net-check
// responder sits on 127.0.0.2:80 — a different loopback address from minerd's, so they never clash. The
// address set comes from lanIPv4s(), which enumerates the real up, non-loopback interfaces: on the Box II
// that is eth0 only (the unit is wired-only, there is no Wi-Fi chip and so no Wi-Fi interface), with nothing
// here hard-coding an interface name either way.
func followLAN(h http.Handler, port string, stop <-chan struct{}) {
	open := map[string]net.Listener{}
	defer func() {
		for ip, ln := range open {
			ln.Close()
			log.Printf("panel: stopped on %s:%s (switched off)", ip, port)
		}
	}()
	for {
		want := map[string]bool{}
		for _, ip := range lanIPv4s() {
			want[ip.String()] = true
		}
		for ip, ln := range open {
			if !want[ip] {
				ln.Close()
				delete(open, ip)
				log.Printf("panel: stopped on %s:%s (address gone)", ip, port)
			}
		}
		for ip := range want {
			if open[ip] != nil {
				continue
			}
			ln, err := net.Listen("tcp4", net.JoinHostPort(ip, port))
			if err != nil {
				continue // tried again next round
			}
			open[ip] = ln
			log.Printf("panel: also on http://%s/", ip)
			go servePanel(ln, h, false)
		}
		select {
		case <-stop:
			return
		case <-time.After(15 * time.Second):
		}
	}
}

// netCheckAddr: xminerd's "internet" check (every 3 s). Stock, it checks a pool host or www.baidu.com /
// www.google.com on port 80 and kills or restarts the miner when none answers, even with a working pool on the
// LAN. This firmware replaces both outside hosts in xminerd with this address and asks it first, so the check
// never leaves the miner and mining doesn't depend on the internet. It only accepts and closes: nothing is served.
var netCheckAddr = "127.0.0.2:80"

func netCheckResponder() {
	for {
		ln, err := net.Listen("tcp4", netCheckAddr)
		if err != nil {
			time.Sleep(30 * time.Second)
			continue
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				time.Sleep(time.Second) // (out of file handles, say): not a hot loop
				break
			}
			c.SetDeadline(time.Now().Add(2 * time.Second))
			c.Write([]byte("HTTP/1.0 204 No Content\r\nConnection: close\r\n\r\n"))
			c.Close()
		}
		ln.Close()
	}
}

// ---- per-address connection limit, local network only ----

var (
	connMu    sync.Mutex
	connBy    = map[string]int{}
	connTotal int
	connOwner = map[string]time.Time{}      // addresses signed in within the last day: 32 more slots past the total
	connLive  = map[*countedConn]struct{}{} // the open ones (access.go closes those from outside when that is switched off)
)

// connMarkOwner: ip just signed in or used a valid session (the owner's browser keeps a way in even when
// another device on the network holds all 256 connections).
func connMarkOwner(ip string) {
	k := failKey(ip)
	connMu.Lock()
	defer connMu.Unlock()
	if t, ok := connOwner[k]; ok && time.Since(t) < time.Minute {
		return
	}
	connOwner[k] = time.Now()
	for o, t := range connOwner {
		if time.Since(t) > 24*time.Hour {
			delete(connOwner, o)
		}
	}
	if len(connOwner) > 16 { // keep the newest few
		var old string
		for o, t := range connOwner {
			if old == "" || t.Before(connOwner[old]) {
				old = o
			}
		}
		delete(connOwner, old)
	}
}

type countedConn struct {
	net.Conn
	key  string
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() {
		connMu.Lock()
		delete(connLive, c)
		connBy[c.key]--
		if connBy[c.key] <= 0 {
			delete(connBy, c.key)
		}
		connTotal--
		connMu.Unlock()
	})
	return c.Conn.Close()
}

// connAllow: the connection counted (closing it uncounts it), or false (and closed) for a peer outside the
// local network (peerAllowed: IPv6 always; IPv4 too on the SC Box II unless "Allow access from outside the local
// network" is on), or an address with 48 open connections already (256 in all).
func connAllow(c net.Conn) (net.Conn, bool) {
	ip := remoteIPOf(c.RemoteAddr())
	if ip != nil && !peerAllowed(ip) {
		c.Close()
		return c, false
	}
	key := "?"
	if ip != nil {
		key = failKey(ip.String())
	}
	connMu.Lock()
	limit := 256
	if t, ok := connOwner[key]; ok && time.Since(t) < 24*time.Hour {
		limit = 256 + 32
	}
	if connBy[key] >= 48 || connTotal >= limit {
		connMu.Unlock()
		c.Close()
		return c, false
	}
	connBy[key]++
	connTotal++
	cc := &countedConn{Conn: c, key: key}
	connLive[cc] = struct{}{}
	connMu.Unlock()
	return cc, true
}

var netInterfaceAddrs = net.InterfaceAddrs // (tests)

func remoteIPOf(a net.Addr) net.IP {
	if t, ok := a.(*net.TCPAddr); ok {
		return t.IP
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(h)
}

// localPeer: loopback, private (RFC 1918, CGNAT, link-local, IPv6 ULA / link-local), or an address inside a
// prefix of one of this miner's own interfaces (the same LAN: a global IPv6 /64, or an IPv4 LAN that uses public
// addresses, so its owner is never locked out). The panel and SSH aren't for the internet.
func localPeer(ip net.IP) bool { return sshcore.LocalPeer(ip, netInterfaceAddrs) }
