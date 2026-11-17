// Package paths resolves where Berth keeps its sockets and database, and finds the binaries
// installed beside the running one.
package paths

import (
	"os"
	"path/filepath"
	"strconv"
)

// Home is $BERTH_HOME, or ~/Library/Application Support/Berth.
func Home() (string, error) {
	if h := os.Getenv("BERTH_HOME"); h != "" {
		return h, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, "Library", "Application Support", "Berth"), nil
}

func Socket(home string) string   { return filepath.Join(home, "berthd.sock") }
func Database(home string) string { return filepath.Join(home, "berth.db") }

// AgentSocket is where berthd serves the agent user by default. It sits outside the owner's
// home, whose ~/Library is 0700, so another user can reach it; one folder per owner keeps two
// owners on one Mac apart.
func AgentSocket(ownerUID int) string {
	return filepath.Join("/Users/Shared/Berth", strconv.Itoa(ownerUID), "agent.sock")
}

// Beside returns name in the folder of the running executable, with symlinks resolved, so a
// helper installed next to berthd or berth is found wherever the pair was put.
func Beside(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), name), nil
}
