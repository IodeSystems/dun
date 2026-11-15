package dund

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestAPITokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Service{dir: dir}
	tok, err := CreateAPIToken(dir, "phone")
	if err != nil || !strings.HasPrefix(tok, apiTokenPrefix) {
		t.Fatalf("create: %q %v", tok, err)
	}
	if _, err := CreateAPIToken(dir, "phone"); !errors.Is(err, ErrTokenExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := CreateAPIToken(dir, "bad name"); err == nil {
		t.Fatal("bad name accepted")
	}
	if !s.CheckAPIToken(tok) || s.CheckAPIToken(tok+"x") || s.CheckAPIToken("") {
		t.Fatal("check")
	}
	list, err := ListAPITokens(dir)
	if err != nil || len(list) != 1 || list[0].Name != "phone" || list[0].Hash == tok {
		t.Fatalf("list: %v %v", list, err)
	}
	raw, _ := os.ReadFile(apiTokensPath(dir))
	if strings.Contains(string(raw), tok) {
		t.Fatal("secret stored")
	}
	if fi, _ := os.Stat(apiTokensPath(dir)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if err := RevokeAPIToken(dir, "phone"); err != nil {
		t.Fatal(err)
	}
	if s.CheckAPIToken(tok) {
		t.Fatal("revoked token still works")
	}
	if err := RevokeAPIToken(dir, "phone"); !errors.Is(err, ErrNoSuchToken) {
		t.Fatalf("revoke twice: %v", err)
	}
}
