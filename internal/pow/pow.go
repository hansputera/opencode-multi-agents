// Package pow implements a Hashcash-style proof-of-work gate for API key
// issuance: the server issues single-use, time-limited, client-bound
// challenges; clients find a nonce whose BLAKE3 (or SHA-256) hash has a
// required number of leading zero bits; verification on the server is one
// hash plus two indexed SQLite statements.
//
// Protocol (v1):
//
//	preimage  = "pow-v1|<id>|<resource>|<algo>|<difficulty>|<salt>|<bind>|<counter>"
//	solution  = valid iff leadingZeroBits(H(preimage)) >= difficulty
//	H         = BLAKE3-256 or SHA-256 (challenge.algo decides)
package pow

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	"github.com/zeebo/blake3"
)

// Version is the protocol version baked into every preimage. Bump it to
// invalidate all outstanding challenges when the construction changes.
const Version = 1

// Supported proof-of-work hash algorithms.
const (
	AlgoBLAKE3 = "blake3" // preferred (native clients, server-side)
	AlgoSHA256 = "sha256" // fallback / browser + WebGPU path
)

// Resource identifiers bind a challenge to what it unlocks.
const ResourceAPIKey = "api-key"

// Plan tiers. Difficulty bits are ADDED to the adaptive global base.
var Plans = []string{"basic", "plus", "pro"}

// ValidPlan reports whether plan is a known tier name.
func ValidPlan(plan string) bool {
	switch plan {
	case "basic", "plus", "pro":
		return true
	}
	return false
}

// ValidAlgo reports whether algo is supported.
func ValidAlgo(algo string) bool {
	return algo == AlgoBLAKE3 || algo == AlgoSHA256
}

// Challenge is issued to a client and must be solved before redemption.
type Challenge struct {
	Version    int    `json:"version"`
	ID         string `json:"id"`
	Resource   string `json:"resource"`
	Algo       string `json:"algo"`
	Difficulty int    `json:"difficulty"`
	Salt       string `json:"salt"` // base64 server random
	Bind       string `json:"bind"` // base64 client binding (hashed IP+fingerprint)
	IssuedAt   int64  `json:"issued_at"`
	ExpiresAt  int64  `json:"expires_at"`

	// Plan is the tier this challenge unlocks (not part of the preimage —
	// it is enforced server-side from stored state).
	Plan string `json:"plan"`
}

// Preimage builds the canonical string that gets hashed. The format MUST stay
// byte-stable within a protocol version; JS/WGSL solvers replicate it exactly.
func (c *Challenge) Preimage(counter string) string {
	return fmt.Sprintf("pow-v%d|%s|%s|%s|%d|%s|%s|%s",
		c.Version, c.ID, c.Resource, c.Algo, c.Difficulty, c.Salt, c.Bind, counter)
}

// Digest hashes a preimage with the challenge's algorithm and returns the
// raw 32-byte digest.
func (c *Challenge) Digest(preimage string) [32]byte {
	var out [32]byte
	if c.Algo == AlgoSHA256 {
		out = sha256.Sum256([]byte(preimage))
	} else {
		out = blake3.Sum256([]byte(preimage))
	}
	return out
}

// LeadingZeroBits counts the number of leading zero bits of a big-endian
// digest — the difficulty metric.
func LeadingZeroBits(digest [32]byte) int {
	bits := 0
	for _, b := range digest { // big-endian byte order: [0] is most significant
		if b == 0 {
			bits += 8
			continue
		}
		for shift := 7; shift >= 0; shift-- {
			if b&(1<<uint(shift)) != 0 {
				return bits
			}
			bits++
		}
	}
	return bits
}

// Satisfies reports whether digest meets the difficulty target.
func Satisfies(digest [32]byte, difficulty int) bool {
	return LeadingZeroBits(digest) >= difficulty
}

// Verify checks counters against the challenge and returns nil for the first
// counter that satisfies the target. It never trusts any client metadata.
func Verify(c *Challenge, counters []string) (int, error) {
	if len(counters) == 0 {
		return -1, errors.New("no solution provided")
	}
	if len(counters) > 8 {
		counters = counters[:8] // hard cap: verification stays O(1)
	}
	for _, ctr := range counters {
		if len(ctr) == 0 || len(ctr) > 20 {
			continue
		}
		d := c.Digest(c.Preimage(ctr))
		if Satisfies(d, c.Difficulty) {
			return LeadingZeroBits(d), nil
		}
	}
	return -1, fmt.Errorf("solution does not meet difficulty %d", c.Difficulty)
}

// NewSalt returns base64-encoded cryptographic random bytes.
func NewSalt(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// BindingHash derives the client-binding token from the request IP and a
// light fingerprint (User-Agent). Only the hash is ever stored or sent so no
// PII appears in challenges or the database.
func BindingHash(ip, userAgent string) string {
	h := blake3.Sum256([]byte("pow-bind-v1|" + ip + "|" + userAgent))
	return base64.RawStdEncoding.EncodeToString(h[:16])
}

// CounterToBytes renders an integer counter as fixed-width decimal ASCII —
// the representation both the browser GPU kernel and native solver use, so
// the preimage is byte-identical across implementations.
func CounterToBytes(counter uint64) [10]byte {
	var out [10]byte
	s := strconv.FormatUint(counter, 10)
	for i := range out {
		out[i] = '0'
	}
	copy(out[10-len(s):], s)
	return out
}

// CounterFromBytes parses fixed-width decimal ASCII back into a uint64.
func CounterFromBytes(b []byte) (uint64, error) {
	if len(b) != 10 {
		return 0, errors.New("counter must be 10 ASCII digits")
	}
	v, err := strconv.ParseUint(string(b), 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// KeyHash is how issued keys are identified at rest: only SHA-256 of the
// bearer token is stored, never the key itself.
func KeyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return base64.RawStdEncoding.EncodeToString(h[:])
}

// u64BE is a helper used by tests to build deterministic digests.
func u64BE(v uint64) [32]byte {
	var d [32]byte
	binary.BigEndian.PutUint64(d[:8], v)
	return d
}
