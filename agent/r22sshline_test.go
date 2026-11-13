package main

import (
	"bytes"
	"strings"
	"testing"
)

type rwBuf struct {
	in  *strings.Reader
	out bytes.Buffer
}

func (b *rwBuf) Read(p []byte) (int, error)  { return b.in.Read(p) }
func (b *rwBuf) Write(p []byte) (int, error) { return b.out.Write(p) }

// the forced password change over SSH: what the owner types is what gets saved
func TestR22SSHLineReader(t *testing.T) {
	read := func(in string, n int) []string {
		r := &sshLineReader{rw: &rwBuf{in: strings.NewReader(in)}}
		var got []string
		for i := 0; i < n; i++ {
			l, long, ok := r.read()
			if !ok {
				got = append(got, "<eof>")
				break
			}
			if long {
				l = "<long>"
			}
			got = append(got, l)
		}
		return got
	}
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"hunter2hunter2\r\nhunter2hunter2\r\n", 2, "hunter2hunter2|hunter2hunter2"}, // CR LF: one Enter
		{"abc\rdef\n", 2, "abc|def"},
		{"abc\n\ndef\n", 3, "abc||def"},                                       // a real empty line still counts
		{"pässwörd\x7f\x7fd\r", 1, "pässwöd"}, {"pässwö\x7fd\r", 1, "pässwd"}, // Backspace removes whole characters
		{"pass\x1b[Dword\x1bOA!\r", 1, "password!"},           // arrows ignored
		{strings.Repeat("x", 300) + "\rok\r", 2, "<long>|ok"}, // too long: said, not cut
		{strings.Repeat("y", 256) + "\r", 1, strings.Repeat("y", 256)},
		{"abc\x03", 1, "<eof>"},
	} {
		if got := strings.Join(read(c.in, c.n), "|"); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
