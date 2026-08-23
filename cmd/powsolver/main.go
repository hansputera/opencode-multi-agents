// Command powsolver is a native multi-core client for the gateway's
// PoW-gated API key issuance. It fetches a challenge, solves it using all
// logical CPU cores (BLAKE3 preferred, SHA-256 available), redeems the
// solution, and prints the issued API key.
//
// Examples:
//
//	powsolver --server http://localhost:8082 --plan basic
//	powsolver --server https://gw.example.com --plan pro --algo blake3 --workers 16
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crypto/sha256"
	"github.com/zeebo/blake3"
)

const (
	algoBlake3 = "blake3"
	algoSHA256 = "sha256"
)

type challenge struct {
	Version    int    `json:"version"`
	ID         string `json:"id"`
	Resource   string `json:"resource"`
	Algo       string `json:"algo"`
	Difficulty int    `json:"difficulty"`
	Salt       string `json:"salt"`
	Bind       string `json:"bind"`
	IssuedAt   int64  `json:"issued_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

func (c *challenge) preimage(counter uint64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "pow-v%d|%s|%s|%s|%d|%s|%s|%010d",
		c.Version, c.ID, c.Resource, c.Algo, c.Difficulty, c.Salt, c.Bind, counter)
	return []byte(b.String())
}

func (c *challenge) digest(pre []byte) [32]byte {
	if c.Algo == algoSHA256 {
		return sha256.Sum256(pre)
	}
	return blake3.Sum256(pre)
}

func sha256Sum(b []byte) [32]byte {
	return sha256.Sum256(b)
}

func main() {
	server := flag.String("server", "http://localhost:8082", "gateway base URL")
	plan := flag.String("plan", "basic", "plan: basic | plus | pro")
	algo := flag.String("algo", algoBlake3, "hash algorithm: blake3 | sha256")
	workers := flag.Int("workers", runtime.NumCPU(), "number of CPU solver goroutines")
	flag.Parse()

	if !validAlgo(*algo) {
		fatal("unsupported algorithm %q (use blake3 or sha256)", *algo)
	}

	ch := fetchChallenge(*server, *plan, *algo)
	expected := uint64(1) << ch.Difficulty
	fmt.Printf("challenge : %s\n", ch.ID)
	fmt.Printf("plan      : %s  difficulty: %d bits (%s)\n", planLabel(ch), ch.Difficulty, ch.Algo)
	fmt.Printf("expected  : ~%d hashes across %d workers\n", expected, *workers)
	warnHeavy(ch.Difficulty)

	// Solve: worker i scans counters lane = i mod workers.
	var found atomic.Uint64
	var done atomic.Bool
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(lane int) {
			defer wg.Done()
			counter := uint64(lane)
			local := 0
			last := time.Now()
			for !done.Load() {
				pre := ch.preimage(counter)
				var d [32]byte
				if ch.Algo == algoSHA256 {
					d = sha256Sum(pre)
				} else {
					d = blake3.Sum256(pre)
				}
				if leadingZeroBits(d) >= ch.Difficulty {
					found.Store(counter)
					done.Store(true)
					return
				}
				counter += uint64(*workers)
				local++
				if local%200000 == 0 {
					if dt := time.Since(last); dt > 500*time.Millisecond {
						hps := float64(local) / dt.Seconds()
						tried := atomicTotal.Load()
						fmt.Printf("\r  %10d H/s | tried ~%d | ETA %s   ",
							int(hps), tried, eta(expected, hps))
						last = time.Now()
						local = 0
					}
				}
				atomicTotal.Add(1)
			}
		}(w)
	}
	wg.Wait()
	if found.Load() == 0 && !solvedFlag() {
		fatal("\nno solution found")
	}
	fmt.Printf("\rsolved in %s%s\n", time.Since(start).Round(time.Millisecond), spaces(8))

	key := redeem(*server, ch.ID, found.Load())
	fmt.Printf("\nAPI key   : %s\n(key is shown once — store it safely)\n", key)
}

// --- shared state for progress reporting ---
var atomicTotal atomic.Uint64

func solvedFlag() bool { return false }

func validAlgo(a string) bool { return a == algoBlake3 || a == algoSHA256 }

func leadingZeroBits(d [32]byte) int {
	bits := 0
	for _, b := range d {
		if b == 0 {
			bits += 8
			continue
		}
		for s := 7; s >= 0; s-- {
			if b&(1<<uint(s)) != 0 {
				return bits
			}
			bits++
		}
	}
	return bits
}

func eta(expected uint64, hps float64) time.Duration {
	if hps <= 0 {
		return time.Duration(1<<62 - 1)
	}
	remaining := float64(expected) - float64(atomicTotal.Load())
	if remaining < 0 {
		remaining = 0
	}
	return time.Duration(remaining / hps * float64(time.Second)).Round(time.Second)
}

func warnHeavy(difficulty int) {
	if difficulty > 28 {
		fmt.Println("⚠️  This difficulty will keep ALL your CPU cores at 100% for a long time.")
	}
}

func spaces(n int) string { return strings.Repeat(" ", n) }

func planLabel(c *challenge) string {
	switch c.Resource {
	default:
		return c.Resource
	}
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// --- HTTP helpers ---

func fetchChallenge(server, plan, algo string) *challenge {
	url := fmt.Sprintf("%s/api/pow/challenge?plan=%s&algo=%s", strings.TrimRight(server, "/"), plan, algo)
	resp, err := http.Get(url)
	must(err, "fetching challenge")
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		fatal("challenge endpoint returned %d: %s", resp.StatusCode, body)
	}
	var wrap struct {
		Challenge challenge `json:"challenge"`
	}
	must(json.Unmarshal(body, &wrap), "parsing challenge")
	return &wrap.Challenge
}

func redeem(server, challengeID string, counter uint64) string {
	payload := fmt.Sprintf(`{"challenge_id":%q,"counters":["%010d"]}`, challengeID, counter)
	resp, err := http.Post(strings.TrimRight(server, "/")+"/api/pow/redeem", "application/json", strings.NewReader(payload))
	must(err, "redeeming solution")
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		APIKey string `json:"api_key"`
		Error  *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	must(json.Unmarshal(body, &parsed), "parsing redemption response")
	if parsed.Error != nil {
		fatal("redeem rejected (%s): %s", parsed.Error.Code, parsed.Error.Message)
	}
	return parsed.APIKey
}

func must(err error, what string) {
	if err != nil {
		fatal("%s: %v", what, err)
	}
}
