// Package paths resolves where Berth keeps its socket and database.
package paths

import (
	"os"
	"path/filepath"
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
