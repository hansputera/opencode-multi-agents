package pow

import (
	"crypto/sha256"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/pow.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestChallengeLifecycle(t *testing.T) {
	s := newTestStore(t)
	sc := &StoredChallenge{
		ID: "c1", Bind: "bind", Plan: "basic", Algo: AlgoSHA256,
		Difficulty: 20, Salt: "salt",
		IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	if err := s.InsertChallenge(sc); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetChallenge("c1")
	if err != nil || got == nil {
		t.Fatalf("get: %v, %v", got, err)
	}
	if got.Used || got.Plan != "basic" || got.Difficulty != 20 {
		t.Errorf("stored challenge mismatch: %+v", got)
	}

	ok, err := s.ConsumeChallenge("c1")
	if err != nil || !ok {
		t.Fatalf("first consume = %v, %v; want true", ok, err)
	}
	ok, err = s.ConsumeChallenge("c1")
	if err != nil || ok {
		t.Fatalf("second consume = %v, %v; want false (single-use)", ok, err)
	}

	ch := got.ToChallenge()
	if ch.Version != Version || ch.Resource != ResourceAPIKey {
		t.Errorf("ToChallenge mismatch: %+v", ch)
	}
}

func TestConsumeExpiredFails(t *testing.T) {
	s := newTestStore(t)
	sc := &StoredChallenge{
		ID: "exp", Bind: "b", Plan: "basic", Algo: AlgoBLAKE3,
		Difficulty: 8, Salt: "s",
		IssuedAt:  time.Now().Add(-time.Hour).Unix(),
		ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}
	if err := s.InsertChallenge(sc); err != nil {
		t.Fatal(err)
	}
	ok, err := s.ConsumeChallenge("exp")
	if err != nil || ok {
		t.Errorf("consuming expired challenge = %v, %v; want false", ok, err)
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	s := newTestStore(t)
	k := APIKey{
		KeyHash:   KeyHash("sk-gw-abc"),
		Prefix:    "sk-gw-abcd",
		Plan:      "plus",
		RPM:       250,
		CreatedAt: time.Now().Unix(),
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour).Unix(),
	}
	if err := s.InsertAPIKey(k); err != nil {
		t.Fatalf("insert key: %v", err)
	}

	keys, err := s.ListAPIKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("list = %d keys, %v", len(keys), err)
	}

	got, err := s.GetAPIKey(k.KeyHash)
	if err != nil || got == nil || got.Plan != "plus" || got.RPM != 250 {
		t.Fatalf("get key = %+v, %v", got, err)
	}

	// Expired keys are invisible.
	expired := k
	expired.KeyHash = KeyHash("sk-gw-old")
	expired.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	if err := s.InsertAPIKey(expired); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetAPIKey(expired.KeyHash)
	if got2 != nil {
		t.Error("expired key must not resolve")
	}
}

func TestIPBonusCapAndDecay(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 12; i++ {
		bits, err := s.BumpIPBonus("iphash", 8)
		if err != nil {
			t.Fatalf("bump %d: %v", i, err)
		}
		want := i + 1
		if want > 8 {
			want = 8
		}
		if bits != want {
			t.Fatalf("bonus after bump %d = %d, want %d (capped at 8)", i, bits, want)
		}
	}
	if got := s.IPBonus("unknown"); got != 0 {
		t.Errorf("unknown ip bonus = %d, want 0", got)
	}
}

func TestSHA256DigestMatchesGo(t *testing.T) {
	c := &Challenge{Algo: AlgoSHA256}
	d := c.Digest("hello")
	ref := sha256.Sum256([]byte("hello"))
	if d != ref {
		t.Error("sha256 digest path broken")
	}
}
