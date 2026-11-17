// Package ownerchain decides who started a browser by walking its parents. It is a guess from
// the process tree, made by pure rules: an agent anywhere above the browser wins, then the
// nearest terminal, then the nearest app.
package ownerchain

import (
	"path"
	"strings"

	"berth/internal/core/ports"
)

type Kind string

const (
	Agent    Kind = "Agent"
	Terminal Kind = "Terminal"
	App      Kind = "App"
	// Person is a browser app launchd started, from the Dock, Finder or open.
	Person Kind = "Person"
	// None is an automated or headless browser already hanging from launchd: its owner exited.
	None Kind = "None"
	// Process is the browser's parent when no rule recognises any ancestor.
	Process Kind = "Process"
)

// Ancestor is one process above the browser, with its arguments when the kernel gave them.
type Ancestor struct {
	ports.ProcessInfo
	Args []string
}

// Owner is the process the rules picked; Process is zero for Person and None.
type Owner struct {
	Kind    Kind
	Name    string
	Process ports.ProcessInfo
}

const launchdPID = 1

var agents = map[string]bool{
	"claude": true, "codex": true, "cursor-agent": true, "gemini": true, "aider": true,
	"opencode": true, "amp": true, "goose": true,
}

// runtimes run an agent written as a script, named by its first script argument.
var runtimes = map[string]bool{"node": true, "bun": true, "deno": true}

// terminalApps are matched by their bundle, since some name their executable generically (Warp's
// is "stable"); multiplexers by their executable.
var (
	terminalApps = map[string]string{
		"Terminal": "Terminal", "iTerm": "iTerm2", "Ghostty": "Ghostty", "WezTerm": "WezTerm",
		"kitty": "kitty", "Alacritty": "Alacritty", "Warp": "Warp",
	}
	multiplexers = map[string]bool{"tmux": true, "screen": true}
)

// Find picks the owner from the browser's ancestors, parent first, up to launchd. automated says
// whether the browser shows any sign of automation (a driver, headless, or a CDP endpoint), which
// is what separates the person's own browser from a leftover when launchd is the parent.
func Find(chain []Ancestor, automated bool) Owner {
	if len(chain) == 0 || chain[0].PID == launchdPID {
		if automated {
			return Owner{Kind: None}
		}
		return Owner{Kind: Person}
	}
	var terminal, app *Owner
	for _, a := range chain {
		if a.PID == launchdPID {
			break
		}
		if name, ok := agentName(a); ok {
			return Owner{Kind: Agent, Name: name, Process: a.ProcessInfo}
		}
		if name, ok := terminalName(a.Executable); ok && terminal == nil {
			terminal = &Owner{Kind: Terminal, Name: name, Process: a.ProcessInfo}
		}
		if name, ok := appName(a.Executable); ok && app == nil {
			app = &Owner{Kind: App, Name: name, Process: a.ProcessInfo}
		}
	}
	switch {
	case terminal != nil:
		return *terminal
	case app != nil:
		return *app
	}
	return Owner{Kind: Process, Name: path.Base(chain[0].Executable), Process: chain[0].ProcessInfo}
}

// ReadsArgs reports whether Find looks at an ancestor's arguments, so a scan reads only those.
func ReadsArgs(exe string) bool { return runtimes[path.Base(exe)] }

func agentName(a Ancestor) (string, bool) {
	base := path.Base(a.Executable)
	if agents[base] {
		return base, true
	}
	if !ReadsArgs(a.Executable) {
		return "", false
	}
	for _, arg := range a.Args[min(1, len(a.Args)):] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if name := scriptName(arg); agents[name] {
			return name, true
		}
		return "", false
	}
	return "", false
}

// scriptName names an agent run as a script: a bin link (.../bin/gemini) or a package's entry
// point (.../node_modules/@google/gemini-cli/dist/index.js names gemini-cli, then gemini).
func scriptName(arg string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(path.Base(arg), ".js"), ".mjs"), ".cjs")
	if agents[base] {
		return base
	}
	if i := strings.LastIndex(arg, "/node_modules/"); i >= 0 {
		pkg := arg[i+len("/node_modules/"):]
		if strings.HasPrefix(pkg, "@") {
			_, pkg, _ = strings.Cut(pkg, "/")
		}
		pkg, _, _ = strings.Cut(pkg, "/")
		for _, suffix := range []string{"", "-cli", "-code"} {
			if name := strings.TrimSuffix(pkg, suffix); agents[name] {
				return name
			}
		}
	}
	return base
}

func terminalName(exe string) (string, bool) {
	if app, ok := appName(exe); ok {
		name, ok := terminalApps[app]
		return name, ok
	}
	base := path.Base(exe)
	return base, multiplexers[base]
}

// appName is the outermost .app bundle that holds exe, so a helper names its app.
func appName(exe string) (string, bool) {
	i := strings.Index(exe, ".app/")
	if i < 0 {
		return "", false
	}
	return path.Base(exe[:i]), true
}
