// scbox2-sshd: the SC Box II's SSH server, a program of its own so SSH keeps working when the control panel
// (/usr/bin/scbox2-agent) is damaged or crashing. Started before the panel by /etc/init.d/S47scbox2-sshd.
//
// It behaves as the panel's built-in SSH did:
//   - the panel's account: its login file (auth.json), so changing the panel password changes SSH's; while it is
//     still the factory password, an SSH sign-in must choose a new one before it gets a shell (written to the
//     login file as the panel writes it; the panel picks the change up);
//   - this miner's own host key (ssh_host_ed25519_key next to the login file), made on first start once the
//     kernel's random pool is seeded;
//   - local network only unless Settings -> Network allows access from outside; 5 wrong passwords in 10 minutes
//     block an address for 10 minutes; at most 8 connections, 2 not yet signed in per address; keepalives;
//   - a shell, a command, SFTP; no forwarding;
//   - on/off and port from the panel's network settings (net.json), re-read every few seconds and at once on
//     SIGHUP (the panel sends one when they are saved). Its state for the panel's Settings page goes to a file in
//     /tmp; sign-ins and blocks go to an events file the panel turns into its activity log and notifications.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	brand        = "scbox2"
	brandProduct = "SC Box II"
	cfgDir       = "/usr/config/" + brand
)

func main() {
	authPath := flag.String("authfile", cfgDir+"/auth.json", "the control panel's login file (the SSH host key is kept next to it)")
	netPath := flag.String("netcfg", cfgDir+"/net.json", "the control panel's network settings (SSH on/off and port, access from outside)")
	statePath := flag.String("state", "/tmp/"+brand+"-sshd.json", "where this server's state is written for the panel")
	eventsPath := flag.String("events", "/tmp/"+brand+"-sshd.events", "where sign-ins and blocks are written for the panel")
	poll := flag.Duration("poll", 2*time.Second, "how often the settings and the login file are re-read")
	createAfter := flag.Duration("create-login-after", 2*time.Minute, "no login file this long after start (no panel ever made one): make the factory login (admin/admin, must be changed)")
	flag.Parse()
	log.SetFlags(log.LstdFlags)
	log.SetPrefix(brand + "-sshd: ")

	s := newServer(*authPath, *netPath, *statePath, *eventsPath)
	s.createAfter = *createAfter
	s.apply()
	hup := make(chan os.Signal, 4)
	signal.Notify(hup, syscall.SIGHUP)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	signal.Ignore(syscall.SIGPIPE)
	t := time.NewTicker(*poll)
	defer t.Stop()
	for {
		select {
		case <-hup:
			s.apply()
		case <-t.C:
			s.apply()
		case sig := <-term:
			log.Printf("%v: stopping", sig)
			s.stop()
			os.Exit(0)
		}
	}
}
