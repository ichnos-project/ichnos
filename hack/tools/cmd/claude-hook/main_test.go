package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func hookJSON(t *testing.T, command string) string {
	t.Helper()
	data, err := json.Marshal(hookInput{ToolInput: toolInput{Command: command}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRun(t *testing.T) {
	tests := []struct {
		command  string
		wantCode int
		wantText string
	}{
		{`git commit -s -m "fix: x"`, exitBlock, "git commit -s -m fix: x: signs off"},
		{"git commit -s -m \"fix: x\x1b[2K\"", exitBlock, "git commit -s -m fix: x[2K: signs off"},
		{`git commit -m "fix: x"`, exitAllow, ""},
		{`git status`, exitAllow, ""},
		{`git commit -m "unterminated`, exitError, "unterminated quote"},
	}
	for _, tt := range tests {
		var stderr bytes.Buffer
		code := run(nil, strings.NewReader(hookJSON(t, tt.command)), &stderr)
		if code != tt.wantCode {
			t.Errorf("run(%q) = %d, want %d; stderr: %s", tt.command, code, tt.wantCode, stderr.String())
		}
		if !strings.Contains(stderr.String(), tt.wantText) {
			t.Errorf("run(%q) stderr = %q, want it to contain %q", tt.command, stderr.String(), tt.wantText)
		}
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		input string
	}{
		{"an argument", []string{"bash"}, "{}"},
		{"invalid JSON", nil, "{"},
		{"empty input", nil, ""},
	}
	for _, tt := range tests {
		var stderr bytes.Buffer
		code := run(tt.args, strings.NewReader(tt.input), &stderr)
		if code != exitError {
			t.Errorf("%s: run() = %d, want %d", tt.name, code, exitError)
		}
	}
}
