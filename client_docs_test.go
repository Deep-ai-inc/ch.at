package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestReadmeShellHelperQuotesPrompts(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash recipe test requires bash")
	}
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "chat() {")
	if start < 0 {
		t.Fatal("missing shell helper")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("unterminated shell helper")
	}
	function := text[start : start+end+3]
	// Stub curl: inspect its argv without making a network/model request.
	script := "curl() { printf '%s\\0' \"$@\"; }\n" + function + "\nchat \"$TEST_PROMPT\""
	cmd := exec.Command(bash, "-c", script)
	prompt := "Quotes: \"double\" 'single' & café; $(not-a-command)\nsecond line"
	cmd.Env = append(os.Environ(), "TEST_PROMPT="+prompt)
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	args := bytes.Split(bytes.TrimSuffix(output, []byte{0}), []byte{0})
	found := false
	for i, arg := range args {
		if string(arg) == "--data-urlencode" && i+1 < len(args) && string(args[i+1]) == "q="+prompt {
			found = true
		}
	}
	if !found || string(args[len(args)-1]) != "https://ch.at/" {
		t.Fatalf("unexpected curl arguments: %q", args)
	}
	cmd = exec.Command(bash, "-c", "curl() { echo unexpected-network-call; }\n"+function+"\nchat")
	output, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Usage:") || strings.Contains(string(output), "unexpected-network-call") {
		t.Fatalf("empty input: %q %v", output, err)
	}
}
