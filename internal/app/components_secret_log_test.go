package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestCodexAppLogRedactsCredentialAtEverySplit(t *testing.T) {
	secret := "9EfybhYYZw_unique-credential_0haZyz"
	input := "started\npassword: " + secret + "\nurl unchanged\n" + secret + "\n"
	for split := 0; split <= len(input); split++ {
		var dest bytes.Buffer
		writer := &secretLogWriter{dest: &dest, secret: []byte(secret)}
		if n, err := writer.Write([]byte(input[:split])); err != nil || n != split {
			t.Fatalf("first write failed at %d: %v", split, err)
		}
		if _, err := writer.Write([]byte(input[split:])); err != nil {
			t.Fatal(err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
		if dest.String() != strings.ReplaceAll(input, secret, "[REDACTED]") {
			t.Fatalf("redaction or output fidelity failed at split %d", split)
		}
	}
}

func TestCodexAppLogRedactionIsLiveAndBoundsPendingBytes(t *testing.T) {
	var dest bytes.Buffer
	secret := "private-credential"
	writer := &secretLogWriter{dest: &dest, secret: []byte(secret)}
	for _, char := range []byte(strings.Repeat("ordinary log line\n", 1000)) {
		if _, err := writer.Write([]byte{char}); err != nil {
			t.Fatal(err)
		}
		if len(writer.pending) >= len(secret) {
			t.Fatal("redaction retained unbounded output")
		}
	}
	if dest.Len() != len(strings.Repeat("ordinary log line\n", 1000)) {
		t.Fatal("ordinary output not emitted live")
	}
	if _, err := writer.Write([]byte("private-")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dest.String(), "private-") {
		t.Fatal("possible partial credential leaked before next write")
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dest.String(), "[REDACTED]") {
		t.Fatal("partial credential exposed at process exit")
	}
}
