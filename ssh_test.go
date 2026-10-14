package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSSHOutputNewlinesAcrossChunks(t *testing.T) {
	for _, tc := range []struct {
		chunks []string
		want   string
	}{
		{[]string{"one\ntwo\nthree"}, "one\r\ntwo\r\nthree"},
		{[]string{"one\r", "\ntwo\n", "\nthree"}, "one\r\ntwo\r\n\r\nthree"},
		{[]string{"\r", "", "\n", "你好\nworld"}, "\r\n你好\r\nworld"},
	} {
		for _, terminal := range []bool{false, true} {
			var out strings.Builder
			w := sshOutputWriter{w: &out, terminal: terminal}
			for _, chunk := range tc.chunks {
				if err := w.WriteString(chunk); err != nil {
					t.Fatal(err)
				}
			}
			want := tc.want
			if !terminal {
				want = strings.Join(tc.chunks, "")
			}
			if out.String() != want {
				t.Fatalf("PTY=%v got %q want %q", terminal, out.String(), want)
			}
		}
	}
}

type scriptedSSHChannel struct {
	input  *strings.Reader
	output bytes.Buffer
}

func (c *scriptedSSHChannel) Read(p []byte) (int, error)                     { return c.input.Read(p) }
func (c *scriptedSSHChannel) Write(p []byte) (int, error)                    { return c.output.Write(p) }
func (c *scriptedSSHChannel) Close() error                                   { return nil }
func (c *scriptedSSHChannel) CloseWrite() error                              { return nil }
func (c *scriptedSSHChannel) SendRequest(string, bool, []byte) (bool, error) { return true, nil }
func (c *scriptedSSHChannel) Stderr() io.ReadWriter                          { return c }

func TestSSHChatNormalizesModelOutputOnlyForPTY(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		ch := &scriptedSSHChannel{input: strings.NewReader("question\rexit\r")}
		var calls atomic.Int32
		runSSHChatWithLLM(ch, "198.51.100.99:1234", terminal, fixedChatLLM(&calls, "First\nSecond\r", "\nThird"))
		want := "First\nSecond\r\nThird"
		if terminal {
			want = "First\r\nSecond\r\nThird"
		}
		if calls.Load() != 1 || !strings.Contains(ch.output.String(), want) || strings.Contains(ch.output.String(), "\r\r\n") {
			t.Fatalf("PTY=%v calls=%d output=%q", terminal, calls.Load(), ch.output.String())
		}
	}
}

type shortSSHWriter struct{}

func (shortSSHWriter) Write(p []byte) (int, error) { return 0, nil }
func TestSSHOutputShortWrite(t *testing.T) {
	w := sshOutputWriter{w: shortSSHWriter{}, terminal: true}
	if err := w.WriteString("hello\n"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}
