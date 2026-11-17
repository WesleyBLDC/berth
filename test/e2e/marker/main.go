// Command marker stands in for a binary swapped in after berthd checked the granted one: if it
// ever runs, it creates the file named by its first argument.
package main

import "os"

func main() {
	if len(os.Args) > 1 {
		_ = os.WriteFile(os.Args[1], []byte("ran\n"), 0o600) //nolint:gosec // the test names the marker file
	}
}
