package e2e

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestGuardBashHook runs the Claude Code PreToolUse hook on sample commands: exit 2 blocks.
func TestGuardBashHook(t *testing.T) {
	cases := []struct {
		command string
		blocked bool
	}{
		{"security find-generic-password -s gh-token -w", true},
		{"security find-generic-password -a me -s gh-token -g", true},
		{"security find-generic-password -ws gh-token", true},
		{"security find-internet-password -s example.com -w", true},
		{"security add-generic-password -U -a me -s gh-token -w hunter2", true},
		{`security add-generic-password -U -a me -s gh-token -w"hunter2"`, true},
		{"security find-generic-password -s gh-token", false},
		{"security add-generic-password -U -a me -s gh-token -w", false},
		{`berth secret request gh-token --why "deploy"`, false},
		{"berth run --env GH_TOKEN=gh-token -- gh api user", false},
	}
	for _, tc := range cases {
		input, _ := json.Marshal(map[string]any{"tool_input": map[string]string{"command": tc.command}})
		cmd := exec.CommandContext(t.Context(), "../../.claude/hooks/guard-bash.sh")
		cmd.Stdin = strings.NewReader(string(input))
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		blocked := errors.As(err, &exit) && exit.ExitCode() == 2
		if err != nil && !blocked {
			t.Fatalf("%s: hook failed: %v %s", tc.command, err, out)
		}
		if blocked != tc.blocked {
			t.Errorf("%s: blocked %v, want %v (%s)", tc.command, blocked, tc.blocked, out)
		}
		if blocked && (!strings.Contains(string(out), "berth secret request") || strings.Count(string(out), "\n") != 1) {
			t.Errorf("%s: want a one line pointer to berth secret request, got %q", tc.command, out)
		}
	}
}
