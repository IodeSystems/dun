package dund

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Mint, redeem once, then refused; the device token is listed and anchors.
func TestPairCodeOnce(t *testing.T) {
	dir := t.TempDir()
	s := &Service{dir: dir}
	code, exp, err := s.MintPairCode()
	if err != nil || len(code) < 22 || time.Until(exp) > PairTTL || time.Until(exp) < PairTTL-time.Minute {
		t.Fatalf("mint: %q %v %v", code, exp, err)
	}
	from, name, err := s.PairDevice(code)
	if err != nil || !strings.HasPrefix(from, "api:") || !strings.HasPrefix(name, "paired-"+time.Now().Format("2006-01-02")+"-via-qr-") {
		t.Fatalf("pair: %q %q %v", from, name, err)
	}
	if !s.fromLive(nil, from, 0) {
		t.Error("the device token is not live")
	}
	if list, _ := ListAPITokens(dir); len(list) != 1 || list[0].Name != name {
		t.Errorf("list: %v", list)
	}
	if _, _, err := s.PairDevice(code); !errors.Is(err, ErrPairUsed) {
		t.Errorf("second use: %v", err)
	}
	if list, _ := ListAPITokens(dir); len(list) != 1 {
		t.Errorf("a refused code made a token: %v", list)
	}
	if err := RevokeAPIToken(dir, name); err != nil || s.fromLive(nil, from, 0) {
		t.Errorf("revoke: %v", err)
	}
}

func TestPairCodeExpires(t *testing.T) {
	s := &Service{dir: t.TempDir()}
	now := time.Now()
	s.pair.now = func() time.Time { return now }
	code, _, err := s.MintPairCode()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(PairTTL)
	if _, _, err := s.PairDevice(code); !errors.Is(err, ErrPairExpired) {
		t.Errorf("at expiry: %v", err)
	}
}

// A wrong code is refused; pairMaxFails of them end the live codes.
func TestPairCodeWrong(t *testing.T) {
	s := &Service{dir: t.TempDir()}
	code, _, err := s.MintPairCode()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "x", code + "x", strings.ToUpper(code)} {
		if _, _, err := s.PairDevice(bad); !errors.Is(err, ErrPairWrong) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for range pairMaxFails {
		_, _, _ = s.PairDevice("guess")
	}
	if _, _, err := s.PairDevice(code); err == nil {
		t.Error("a code lived through the guesses")
	}
	code, _, _ = s.MintPairCode()
	s.DropPairCodes()
	if _, _, err := s.PairDevice(code); !errors.Is(err, ErrPairWrong) {
		t.Errorf("after drop: %v", err)
	}
}
