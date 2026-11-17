// Command consumer is a compiled stand-in for a tool granted a secret in the end to end tests.
// It prints the SHA-256 of $K, so the test can check the exact value arrived, then $K itself,
// which berth run must mask.
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
	fmt.Fprintf(os.Stdout, "%s\n%s", hex.EncodeToString(sum[:]), v)
}
