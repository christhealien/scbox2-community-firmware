package sshcore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// MustChangeExt: the permissions extension a password callback sets while the account still has its factory
// password (the first shell then has to choose a new one).
const MustChangeExt = "must_change"

// Serve runs a signed-in connection until it ends: keepalives (a client gone without a word is dropped after
// KeepWait), no global requests (no port forwarding), and "session" channels only (shell, command, SFTP). While
// the account has its factory password (conn.Permissions' MustChangeExt), the first shell has to choose a new
// one through forced (false: no shell).
func Serve(conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, forced func(ch ssh.Channel) bool) {
	ServeEvery(conn, chans, reqs, forced, KeepEvery, KeepWait)
}

// ServeEvery: Serve with the keepalive's pace (tests).
func ServeEvery(conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, forced func(ch ssh.Channel) bool, every, wait time.Duration) {
	defer conn.Close()
	go ssh.DiscardRequests(reqs) // no port forwarding, no keepalive replies needed
	// keepalive: drop connections whose client vanished. A client gone without a word (Wi-Fi out of range, a
	// laptop asleep) never answers, and the request alone doesn't fail until TCP gives up (~15 min): the answer
	// is waited for at most wait. The loop ends with the connection (not a minute later).
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ended:
				return
			case <-t.C:
			}
			answer := make(chan error, 1)
			go func() { _, _, err := conn.SendRequest("keepalive@openssh.com", true, nil); answer <- err }()
			select {
			case err := <-answer:
				if err == nil {
					continue
				}
			case <-time.After(wait):
			case <-ended:
				return
			}
			conn.Close()
			return
		}
	}()
	mustChange := new(atomic.Bool)
	mustChange.Store(conn.Permissions != nil && conn.Permissions.Extensions[MustChangeExt] == "1")
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.Prohibited, "only shell, commands and sftp are allowed")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go session(ch, creqs, mustChange, forced)
	}
}

type ptyReq struct {
	Term          string
	Cols, Rows    uint32
	Width, Height uint32
	Modes         string
}

func shellEnv(term string) []string {
	if term == "" {
		term = "vt100"
	}
	return []string{"TERM=" + term, "HOME=/root", "USER=root", "LOGNAME=root", "SHELL=/bin/sh",
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "PS1=\\h:\\w\\$ "}
}

func exitWith(ch ssh.Channel, code int) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(code))
	ch.SendRequest("exit-status", false, b)
	ch.Close()
}

func session(ch ssh.Channel, reqs <-chan *ssh.Request, mustChange *atomic.Bool, forced func(ssh.Channel) bool) {
	var pr *ptyReq
	var ptmx *os.File
	var ptyMu sync.Mutex
	started := false
	gone := make(chan struct{}) // the channel (or the whole connection) closed: hang up what it started
	defer close(gone)
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var p ptyReq
			if ssh.Unmarshal(req.Payload, &p) != nil {
				req.Reply(false, nil)
				continue
			}
			pr = &p
			req.Reply(true, nil)
		case "window-change":
			var wc struct{ Cols, Rows, Width, Height uint32 }
			if ssh.Unmarshal(req.Payload, &wc) == nil {
				ptyMu.Lock()
				if pr != nil {
					pr.Cols, pr.Rows = wc.Cols, wc.Rows
				}
				if ptmx != nil {
					pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(wc.Cols), Rows: uint16(wc.Rows)})
				}
				ptyMu.Unlock()
			}
			if req.WantReply {
				req.Reply(true, nil)
			}
		case "env":
			req.Reply(true, nil) // accepted and ignored (LANG and friends)
		case "shell", "exec", "subsystem":
			if started {
				req.Reply(false, nil)
				continue
			}
			started = true
			var cmdline, subsystem string
			if req.Type == "exec" {
				var x struct{ Command string }
				ssh.Unmarshal(req.Payload, &x)
				cmdline = x.Command
			} else if req.Type == "subsystem" {
				var x struct{ Name string }
				ssh.Unmarshal(req.Payload, &x)
				subsystem = x.Name
				if subsystem != "sftp" {
					req.Reply(false, nil)
					ch.Close()
					return
				}
			}
			req.Reply(true, nil)
			go func(pr *ptyReq) {
				if mustChange.Load() {
					if pr == nil {
						fmt.Fprint(ch.Stderr(), "This miner still has its factory password. Sign in once with a plain 'ssh' (no command)\n"+
							"or open the control panel in a browser, and choose a new password first.\r\n")
						exitWith(ch, 1)
						return
					}
					if forced == nil || !forced(ch) {
						exitWith(ch, 1)
						return
					}
					mustChange.Store(false)
				}
				switch {
				case subsystem == "sftp":
					srv, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory("/root"))
					if err != nil {
						exitWith(ch, 1)
						return
					}
					err = srv.Serve()
					srv.Close()
					if err == nil || err == io.EOF {
						exitWith(ch, 0)
					} else {
						exitWith(ch, 1)
					}
				case pr != nil:
					runPty(ch, cmdline, pr, &ptmx, &ptyMu, gone)
				default:
					runPlain(ch, cmdline, gone)
				}
			}(pr)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func command(cmdline string, term string) *exec.Cmd {
	var c *exec.Cmd
	if cmdline == "" {
		c = exec.Command("/bin/sh", "-l")
	} else {
		c = exec.Command("/bin/sh", "-c", cmdline)
	}
	c.Env = shellEnv(term)
	c.Dir = "/root"
	if _, err := os.Stat(c.Dir); err != nil {
		c.Dir = "/"
	}
	return c
}

// ExitCode: a finished command's exit status as SSH reports it (128+signal when killed).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			return 128 + int(st.Signal())
		}
		return ee.ExitCode()
	}
	return 127
}

func runPty(ch ssh.Channel, cmdline string, pr *ptyReq, ptmxp **os.File, mu *sync.Mutex, gone <-chan struct{}) {
	c := command(cmdline, pr.Term)
	f, err := pty.StartWithSize(c, &pty.Winsize{Cols: uint16(pr.Cols), Rows: uint16(pr.Rows)})
	if err != nil {
		// no terminal available: still give a (plain, line-by-line) shell rather than nothing
		fmt.Fprintf(ch.Stderr(), "(no terminal available: %v; plain shell without line editing)\r\n", err)
		runPlain(ch, cmdline, gone)
		return
	}
	mu.Lock()
	*ptmxp = f
	mu.Unlock()
	out := make(chan struct{})
	go func() { io.Copy(ch, f); close(out) }()
	go io.Copy(f, ch)
	exited := make(chan struct{})
	go hangUpWhenGone(c, gone, exited)
	err = c.Wait()
	close(exited)
	select { // let the last output through (the pty reports EIO once the shell is gone)
	case <-out:
	case <-time.After(time.Second):
	}
	f.Close()
	exitWith(ch, ExitCode(err))
}

// hangUpWhenGone: the client went away while c runs: SIGHUP its process group (as a terminal hang-up
// does), then SIGKILL if it's still there 5 s later.
func hangUpWhenGone(c *exec.Cmd, gone, exited <-chan struct{}) {
	select {
	case <-exited:
		return
	case <-gone:
	}
	select {
	case <-exited:
		return // it had already ended (both closed): what it left running in the background stays
	default:
	}
	if c.Process == nil {
		return
	}
	syscall.Kill(-c.Process.Pid, syscall.SIGHUP)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}

func runPlain(ch ssh.Channel, cmdline string, gone <-chan struct{}) {
	c := command(cmdline, "")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its own group, so a hang-up reaches what it started
	c.Stdout, c.Stderr = ch, ch.Stderr()
	stdin, err := c.StdinPipe()
	if err != nil {
		exitWith(ch, 127)
		return
	}
	if err := c.Start(); err != nil {
		fmt.Fprintf(ch.Stderr(), "could not start: %v\n", err)
		exitWith(ch, 127)
		return
	}
	go func() { io.Copy(stdin, ch); stdin.Close() }()
	exited := make(chan struct{})
	go hangUpWhenGone(c, gone, exited)
	err = c.Wait()
	close(exited)
	exitWith(ch, ExitCode(err))
}

// LineReader reads a typed line (no echo). A CR LF counts as one Enter (the LF isn't the next, empty line);
// Backspace removes a whole character (UTF-8); arrow and other escape sequences are ignored; a line longer than
// MaxLine is reported, not cut short (a cut password wouldn't be the one the owner typed).
type LineReader struct {
	rw     io.ReadWriter
	lastCR bool
}

// MaxLine: the longest line (password) LineReader takes.
const MaxLine = 256

func NewLineReader(rw io.ReadWriter) *LineReader { return &LineReader{rw: rw} }

func (r *LineReader) Read() (line string, tooLong, ok bool) {
	var buf []byte
	b := make([]byte, 1)
	esc := 0 // 1 = after ESC, 2 = inside ESC [ ... (until a final byte 0x40-0x7e)
	for {
		if _, err := r.rw.Read(b); err != nil {
			return "", false, false
		}
		c := b[0]
		if c == '\n' && r.lastCR {
			r.lastCR = false
			continue
		}
		r.lastCR = c == '\r'
		switch {
		case esc == 1:
			if c == '[' || c == 'O' {
				esc = 2
			} else {
				esc = 0
			}
			continue
		case esc == 2:
			if c >= 0x40 && c <= 0x7e {
				esc = 0
			}
			continue
		}
		switch c {
		case '\r', '\n':
			r.rw.Write([]byte("\r\n"))
			return string(buf), len(buf) > MaxLine, true
		case 3, 4: // Ctrl-C, Ctrl-D
			r.rw.Write([]byte("\r\n"))
			return "", false, false
		case 27:
			esc = 1
		case 127, 8: // backspace: the whole last character
			for len(buf) > 0 {
				last := buf[len(buf)-1]
				buf = buf[:len(buf)-1]
				if last < 0x80 || last >= 0xc0 { // an ASCII byte or the first byte of a UTF-8 character
					break
				}
			}
		default:
			if c >= 32 && len(buf) <= MaxLine {
				buf = append(buf, c)
			}
		}
	}
}

// ForcedChange makes the first SSH sign-in choose a new password (the panel's own rules: 8+ characters). save
// stores it (the same account as the control panel's) and returns "" when it did, else the line to show
// (nothing was changed).
func ForcedChange(ch io.ReadWriter, save func(pw string) string) bool {
	io.WriteString(ch, "\r\nThis miner still has its factory password (admin/admin).\r\n"+
		"Choose a new one now. It's also your control panel password.\r\n\r\n")
	lr := NewLineReader(ch)
	for tries := 0; tries < 3; tries++ {
		io.WriteString(ch, "New password: ")
		p1, long, ok := lr.Read()
		if !ok {
			return false
		}
		if long {
			io.WriteString(ch, fmt.Sprintf("That's longer than %d characters: choose a shorter one.\r\n\r\n", MaxLine))
			continue
		}
		if len(p1) < 8 {
			io.WriteString(ch, "It needs at least 8 characters.\r\n\r\n")
			continue
		}
		io.WriteString(ch, "Type it again: ")
		p2, _, ok := lr.Read()
		if !ok {
			return false
		}
		if p1 != p2 {
			io.WriteString(ch, "They don't match.\r\n\r\n")
			continue
		}
		if msg := save(p1); msg != "" {
			io.WriteString(ch, msg)
			return false
		}
		io.WriteString(ch, "Password changed. Use it for SSH and the control panel from now on.\r\n\r\n")
		return true
	}
	io.WriteString(ch, "Password not changed.\r\n")
	return false
}
