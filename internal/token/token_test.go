package token_test

import (
	"testing"

	"berth/internal/token"
)

func TestTokensAreRandomAndHashesMatchOnlyTheirToken(t *testing.T) {
	a, err := token.Hex(16)
	if err != nil {
		t.Fatal(err)
	}
	b, err := token.Base64(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || len(b) != 43 {
		t.Fatalf("lengths %d and %d, want 32 and 43", len(a), len(b))
	}
	if again, _ := token.Hex(16); again == a {
		t.Fatal("two tokens were equal")
	}
	if !token.Matches(a, token.Hash(a)) || token.Matches(b, token.Hash(a)) {
		t.Fatal("a hash matched the wrong token")
	}
}
