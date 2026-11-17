// Command consumer is a compiled stand-in for a tool granted a secret in the end to end tests.
// It prints the SHA-256 of $K, so the test can check the exact value arrived, then the
// variables the tests steer, then $K itself, which berth run must mask.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

func main() {
	v := os.Getenv("K")
	sum := sha256.Sum256([]byte(v))
	fmt.Fprintln(os.Stdout, hex.EncodeToString(sum[:]))
	for _, name := range []string{"E2E_PASS", "NODE_OPTIONS", "GIT_CONFIG_COUNT"} {
		if val, ok := os.LookupEnv(name); ok {
			fmt.Fprintf(os.Stdout, "%s=%s\n", name, val)
		} else {
			fmt.Fprintf(os.Stdout, "%s unset\n", name)
		}
	}
	fmt.Fprint(os.Stdout, v)
}
