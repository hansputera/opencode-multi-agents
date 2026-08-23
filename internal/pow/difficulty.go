package pow

import (
	"sync"
	"time"
)

// Difficulty tracks the adaptive global base difficulty and short-lived
// attack escalation. All methods are goroutine-safe.
//
// Adaptation strategy (server-measured data only — never client claims):
//   - Every redemption records the elapsed time since challenge issuance.
//   - An EWMA of those durations drives the base: solving too fast means the
//     network got faster (or an attacker is sweeping) -> +1 bit; too slow
//     means honest users are suffering -> -1 bit.
//   - Escalation adds temporary bits under challenge-flood conditions; it
//     decays automatically after its window passes.
type Difficulty struct {
	mu            sync.Mutex
	base          int
	min           int
	max           int
	targetMin     float64 // seconds; below this -> harder
	targetMax     float64 // seconds; above this -> easier
	ewma          float64 // seconds, 0 = no data yet
	ewmaCount     int
	escalation    int // extra bits while active
	escalateUntil time.Time
}

// NewDifficulty creates the controller. targetMin/targetMax are seconds of
// the desired solve-time window on the reference device class.
func NewDifficulty(base, min, max int, targetMin, targetMax float64) *Difficulty {
	if min > max {
		min, max = max, min
	}
	if base < min {
		base = min
	}
	if base > max {
		base = max
	}
	return &Difficulty{
		base:      base,
		min:       min,
		max:       max,
		targetMin: targetMin,
		targetMax: targetMax,
	}
}

// Current returns the difficulty for a plan tier: clamped
// base + planBits + escalationBonus. ipBonus is the per-IP farming penalty
// in bits (applied by the caller before this call if desired).
func (d *Difficulty) Current(planBits, ipBonus int) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	total := d.base + planBits + d.escalation + ipBonus
	// Clamp to [min,max] but never let a plan's RELATIVE ordering invert:
	// the floor for higher plans is one bit above the previous plan floor,
	// which falls out naturally because planBits are additive and the total
	// is clamped to max.
	if total < d.min {
		total = d.min
	}
	if total > d.max {
		total = d.max
	}
	return total
}

// Base returns the current adaptive base (for logging/telemetry).
func (d *Difficulty) Base() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.base
}

// RecordSolve feeds one server-measured solve duration into the EWMA and may
// adjust the base by one bit when enough samples exist.
func (d *Difficulty) RecordSolve(elapsed time.Duration) {
	secs := elapsed.Seconds()
	if secs < 0 || secs > 24*3600 {
		return // implausible; ignore (also filters expired-challenge redemptions)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	const alpha = 0.2
	if d.ewmaCount == 0 {
		d.ewma = secs
	} else {
		d.ewma = alpha*secs + (1-alpha)*d.ewma
	}
	d.ewmaCount++

	// Adjust only after a meaningful sample size and at most one bit per
	// interval worth of samples (the caller throttles via RecordSolve being
	// called once per redemption anyway).
	if d.ewmaCount < 5 {
		return
	}
	switch {
	case d.ewma < d.targetMin && d.base < d.max:
		d.base++
		d.ewmaCount = 0 // reset window so we observe the new difficulty's effect
	case d.ewma > d.targetMax && d.base > d.min:
		d.base--
		d.ewmaCount = 0
	}
}

// Escalate activates temporary extra difficulty bits (attack valve).
func (d *Difficulty) Escalate(bits int, duration time.Duration) {
	if bits <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.escalation < bits || time.Now().After(d.escalateUntil) {
		d.escalation = bits
	}
	if duration > d.timeUntilEscalation() || time.Now().After(d.escalateUntil) {
		d.escalateUntil = time.Now().Add(duration)
	}
}

// Escalated reports current escalation bits (auto-expired ones are cleared).
func (d *Difficulty) Escalated() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !time.Now().Before(d.escalateUntil) {
		d.escalation = 0
	}
	return d.escalation
}

func (d *Difficulty) timeUntilEscalation() time.Duration {
	return time.Until(d.escalateUntil)
}
