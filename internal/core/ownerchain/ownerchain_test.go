package ownerchain_test

import (
	"testing"

	"berth/internal/core/ownerchain"
	"berth/internal/core/ports"
)

func anc(pid int, exe string, args ...string) ownerchain.Ancestor {
	return ownerchain.Ancestor{ProcessInfo: ports.ProcessInfo{ProcessRef: ports.ProcessRef{PID: pid}, Executable: exe}, Args: args}
}

var (
	launchd  = anc(1, "/sbin/launchd")
	zsh      = anc(40, "/bin/zsh", "-zsh")
	ghostty  = anc(30, "/Applications/Ghostty.app/Contents/MacOS/ghostty")
	warp     = anc(31, "/Applications/Warp.app/Contents/MacOS/stable")
	tmux     = anc(32, "/opt/homebrew/bin/tmux", "tmux")
	claude   = anc(20, "/Users/u/.local/share/claude/ClaudeCode.app/Contents/MacOS/claude", "claude")
	claudeNt = anc(21, "/Users/u/.local/share/claude/versions/2.1.322", "claude")
	notAgent = anc(22, "/Users/u/.local/share/tool/versions/2.1.322", "tool")
	npmExec  = anc(50, "/opt/homebrew/bin/node", "npm exec @playwright/mcp@latest")
	cursorPl = anc(60, "/Applications/Cursor.app/Contents/Frameworks/Cursor Helper (Plugin).app/Contents/MacOS/Cursor Helper (Plugin)")
	gemini   = anc(70, "/opt/homebrew/bin/node", "node", "--no-warnings", "/opt/homebrew/lib/node_modules/@google/gemini-cli/dist/index.js")
	codexBin = anc(71, "/opt/homebrew/bin/node", "node", "/opt/homebrew/bin/codex")
	pwTest   = anc(80, "/opt/homebrew/bin/node", "node", "/repo/node_modules/.bin/playwright", "test")
)

func TestFind(t *testing.T) {
	cases := []struct {
		name      string
		chain     []ownerchain.Ancestor
		automated bool
		kind      ownerchain.Kind
		owner     string
		pid       int
	}{
		{"agent above a terminal wins", []ownerchain.Ancestor{pwTest, zsh, claude, tmux, ghostty, launchd}, true, ownerchain.Agent, "claude", 20},
		{"Claude Code's native install", []ownerchain.Ancestor{pwTest, zsh, claudeNt, ghostty, launchd}, true, ownerchain.Agent, "claude", 21},
		{"a versioned binary of another tool", []ownerchain.Ancestor{notAgent, launchd}, true, ownerchain.Process, "2.1.322", 22},
		{"agent package run by node", []ownerchain.Ancestor{gemini, zsh, ghostty, launchd}, true, ownerchain.Agent, "gemini", 70},
		{"agent bin link run by node", []ownerchain.Ancestor{codexBin, launchd}, true, ownerchain.Agent, "codex", 71},
		{"nearest terminal", []ownerchain.Ancestor{pwTest, zsh, tmux, ghostty, launchd}, true, ownerchain.Terminal, "tmux", 32},
		{"Warp by its bundle", []ownerchain.Ancestor{zsh, warp, launchd}, false, ownerchain.Terminal, "Warp", 31},
		{"app helper names its app", []ownerchain.Ancestor{npmExec, cursorPl, launchd}, true, ownerchain.App, "Cursor", 60},
		{"person's browser from the Dock", []ownerchain.Ancestor{launchd}, false, ownerchain.Person, "", 0},
		{"orphaned automation", []ownerchain.Ancestor{launchd}, true, ownerchain.None, "", 0},
		{"nothing visible above", nil, true, ownerchain.None, "", 0},
		{"unrecognised parent of a launchd job", []ownerchain.Ancestor{pwTest, launchd}, true, ownerchain.Process, "node", 80},
	}
	for _, c := range cases {
		got := ownerchain.Find(c.chain, c.automated)
		if got.Kind != c.kind || got.Name != c.owner || got.Process.PID != c.pid {
			t.Errorf("%s: got %s %q pid %d; want %s %q pid %d", c.name, got.Kind, got.Name, got.Process.PID, c.kind, c.owner, c.pid)
		}
	}
}
