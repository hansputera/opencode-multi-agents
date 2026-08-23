package pow

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestLeadingZeroBits(t *testing.T) {
	cases := []struct {
		hex  string
		want int
	}{
		{"0000000000000000000000000000000000000000000000000000000000000000", 256},
		{"ff00000000000000000000000000000000000000000000000000000000000000", 0},
		{"fe00000000000000000000000000000000000000000000000000000000000000", 0}, // 11111110
		{"01ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 7},
		{"007fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 9},
	}
	for _, c := range cases {
		var d [32]byte
		b, _ := hex.DecodeString(c.hex)
		copy(d[:], b)
		if got := LeadingZeroBits(d); got != c.want {
			t.Errorf("LeadingZeroBits(%s) = %d, want %d", c.hex[:12], got, c.want)
		}
	}
}

// TestPreimageFormatStability pins the canonical serialization. If this test
// breaks, every outstanding client solver breaks too — bump Version instead.
func TestPreimageFormatStability(t *testing.T) {
	c := &Challenge{
		Version:    1,
		ID:         "test-id",
		Resource:   ResourceAPIKey,
		Algo:       AlgoSHA256,
		Difficulty: 26,
		Salt:       "saltB64",
		Bind:       "bindB64",
	}
	want := "pow-v1|test-id|api-key|sha256|26|saltB64|bindB64|0000000042"
	if got := c.Preimage("0000000042"); got != want {
		t.Fatalf("preimage = %q, want %q", got, want)
	}

	d := c.Digest(want)
	ref := sha256.Sum256([]byte(want))
	if d != ref {
		t.Error("Digest does not match crypto/sha256")
	}
}

func TestVerify(t *testing.T) {
	c := &Challenge{
		Version:    1,
		ID:         "v",
		Resource:   ResourceAPIKey,
		Algo:       AlgoSHA256,
		Difficulty: 8,
		Salt:       "s",
		Bind:       "b",
	}
	// Find a real solution.
	solution := -1
	for i := 0; i < 1<<16 && solution < 0; i++ {
		if Satisfies(c.Digest(c.Preimage(pad(i))), c.Difficulty) {
			solution = i
		}
	}
	if solution < 0 {
		t.Fatal("no solution found in search space")
	}

	if bits, err := Verify(c, []string{pad(solution)}); err != nil || bits < 8 {
		t.Errorf("Verify(valid) = %d, %v; want bits>=8, nil", bits, err)
	}
	if _, err := Verify(c, []string{pad((solution + 1) % (1 << 16))}); err == nil {
		t.Error("Verify(invalid) should fail")
	}
	if _, err := Verify(c, nil); err == nil {
		t.Error("Verify(nil) should fail")
	}
}

func pad(n int) string {
	s := "0000000000"
	v := []byte(s)
	str := itoa(n)
	copy(v[10-len(str):], str)
	return string(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestCounterRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 42, 999999999, 4294967295} {
		b := CounterToBytes(v)
		got, err := CounterFromBytes(b[:])
		if err != nil || got != v {
			t.Errorf("roundtrip(%d) = %d, %v", v, got, err)
		}
	}
	if s := CounterToBytes(42); string(s[:]) != "0000000042" {
		t.Errorf("counter bytes = %q, want zero-padded", s)
	}
}

func TestBindingHashDeterministic(t *testing.T) {
	a := BindingHash("1.2.3.4", "UA/1")
	b := BindingHash("1.2.3.4", "UA/1")
	c := BindingHash("1.2.3.5", "UA/1")
	if a != b {
		t.Error("binding hash should be deterministic")
	}
	if a == c {
		t.Error("different IPs must produce different bindings")
	}
	if len(a) != 22 { // 16 bytes raw std b64
		t.Errorf("binding length = %d, want 22", len(a))
	}
}

func TestDifficultyClampAndAdjust(t *testing.T) {
	d := NewDifficulty(24, 20, 30, 30, 90)
	if got := d.Current(0, 0); got != 24 {
		t.Errorf("initial current = %d, want 24", got)
	}
	// Plan bits stack and clamp to max.
	if got := d.Current(100, 100); got != 30 {
		t.Errorf("clamped current = %d, want 30 (max)", got)
	}
	// Fast solves push base up after enough samples.
	for i := 0; i < 10; i++ {
		d.RecordSolve(2 * time.Second)
	}
	if d.Base() != 26 {
		t.Errorf("base after fast solves = %d, want 26 (+1 bit per 5 samples)", d.Base())
	}
	// Slow solves push back down symmetrically.
	for i := 0; i < 10; i++ {
		d.RecordSolve(300 * time.Second)
	}
	if d.Base() != 24 {
		t.Errorf("base after slow solves = %d, want 24", d.Base())
	}
	// Escalation adds temporary bits and expires.
	d.Escalate(3, 50*time.Millisecond)
	if d.Escalated() != 3 {
		t.Error("escalation should be active right away")
	}
	time.Sleep(60 * time.Millisecond)
	if d.Escalated() != 0 {
		t.Error("escalation should expire")
	}
}

func TestKeyHashStable(t *testing.T) {
	k := KeyHash("sk-gw-test")
	if k != KeyHash("sk-gw-test") {
		t.Error("key hash unstable")
	}
	if len(k) != 43 { // 32 bytes raw url-safe b64
		t.Errorf("hash length = %d", len(k))
	}
}
