package main

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzReadMessage checks sendmail's stdin reader: with -i the message is
// read verbatim; otherwise it is the input up to (not including) the first
// line holding only ".".
func FuzzReadMessage(f *testing.F) {
	for _, seed := range []string{
		"",
		".",
		".\r\n",
		plainMessage,
		"Subject: s\n\nbefore\n.\nafter\n",
		"Subject: s\r\n\r\nline\r\n..\r\n.\r\n",
		"no newline at end",
		"a\n.x\n. \n.\n",
	} {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}
	f.Fuzz(func(t *testing.T, in []byte, ignoreDots bool) {
		got, err := readMessage(bytes.NewReader(in), ignoreDots)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ignoreDots {
			if !bytes.Equal(got, in) {
				t.Fatalf("-i changed the message: %q -> %q", in, got)
			}
			return
		}
		if !bytes.HasPrefix(in, got) {
			t.Fatalf("output %q is not a prefix of input %q", got, in)
		}
		lines := strings.SplitAfter(string(got), "\n")
		for _, l := range lines {
			if strings.TrimRight(l, "\r\n") == "." {
				t.Fatalf("output %q kept the terminating dot line", got)
			}
		}
		if len(got) < len(in) {
			// Reading stopped early, so the next line must be the dot.
			rest := string(in[len(got):])
			first, _, _ := strings.Cut(rest, "\n")
			if strings.TrimRight(first, "\r") != "." {
				t.Fatalf("stopped before %q, which is not a dot line", rest)
			}
		}
	})
}

// BenchmarkReadMessage measures reading a typical message from sendmail's
// stdin with dot termination, the default mode.
func BenchmarkReadMessage(b *testing.B) {
	msg := []byte(plainMessage + strings.Repeat("A line of body text in a cron report.\n", 200) + ".\n")
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := readMessage(bytes.NewReader(msg), false); err != nil {
			b.Fatal(err)
		}
	}
}
