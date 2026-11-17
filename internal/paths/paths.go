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
