package dundcli

import (
	"testing"
)

// The CLI round trip: create prints the token, list shows the name, revoke
// removes it, and a duplicate create fails.
func TestTokenCommands(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if Main([]string{"token", "create", "ci"}) != 0 {
		t.Fatal("create")
	}
	if Main([]string{"token", "create", "ci"}) == 0 {
		t.Fatal("duplicate create succeeded")
	}
	if Main([]string{"token", "list"}) != 0 {
		t.Fatal("list")
	}
	if Main([]string{"token", "revoke", "ci"}) != 0 {
		t.Fatal("revoke")
	}
	if Main([]string{"token", "revoke", "ci"}) == 0 {
		t.Fatal("second revoke succeeded")
	}
}
