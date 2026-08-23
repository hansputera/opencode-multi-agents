package handler

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/hansputera/opencode-multi-agents/internal/pow"
	"github.com/rs/zerolog"
)

// PoWService implements the PoW-gated API key issuance flow: challenge
// issuance, solution verification, key issuance, adaptive difficulty, and all
// in-memory rate limiting (challenge quotas, per-key RPM buckets, burst
// cooldowns). State that must survive restarts lives in SQLite; the hot path
// is memory-only.
type PoWService struct {
	cfg   *config.Config
	store *pow.Store
	diff  *pow.Difficulty
	log   *zerolog.Logger

	mu       sync.Mutex
	keyCache map[string]pow.APIKey // keyHash -> key (warmed at startup, added on issue)

	chBuckets map[string]*tokenBucket // clientIP -> challenge-rate bucket
	chDaily   map[string]*dailyCount  // clientIP -> daily challenge counter

	rpmBuckets map[string]*rpmBucket // keyHash -> plan RPM bucket
	burstWin   map[string]*burstWin  // keyHash -> 1s sliding window
	cooldowns  map[string]time.Time  // keyHash -> cooldown-until

	escWindowStart time.Time
	escWindowCount int
}

func newPoWService(cfg *config.Config, log *zerolog.Logger) (*PoWService, error) {
	store, err := pow.Open(cfg.PowStorePath)
	if err != nil {
		return nil, err
	}
	s := &PoWService{
		cfg:        cfg,
		store:      store,
		diff:       pow.NewDifficulty(cfg.PowBaseDifficulty, cfg.PowMinDifficulty, cfg.PowMaxDifficulty, 30, 90),
		log:        log,
		keyCache:   make(map[string]pow.APIKey),
		chBuckets:  make(map[string]*tokenBucket),
		chDaily:    make(map[string]*dailyCount),
		rpmBuckets: make(map[string]*rpmBucket),
		burstWin:   make(map[string]*burstWin),
		cooldowns:  make(map[string]time.Time),
	}

	// Warm the auth cache so lookups are pure memory.
	keys, err := store.ListAPIKeys()
	if err != nil {
		store.Close()
		return nil, err
	}
	for _, k := range keys {
		s.keyCache[k.KeyHash] = k
	}
	s.log.Info().Int("keys", len(s.keyCache)).Msg("PoW service started")

	go s.maintenanceLoop()
	return s, nil
}

// maintenanceLoop prunes expired rows on a schedule. Difficulty adaptation
// happens per-redemption inside RecordSolve; this loop only housekeeps.
func (s *PoWService) maintenanceLoop() {
	interval := s.cfg.PowAdjustInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.store.CleanupExpired(); err != nil {
			s.log.Warn().Err(err).Msg("PoW cleanup failed")
		}
	}
}

// Close releases the store (process shutdown).
func (s *PoWService) Close() error { return s.store.Close() }

// LookupKey resolves an issued key from cache, falling back to SQLite once
// (and repopulating the cache). Returns plan+rpm for the limiter.
func (s *PoWService) LookupKey(keyHash string) (plan string, rpm int, ok bool) {
	s.mu.Lock()
	k, hit := s.keyCache[keyHash]
	s.mu.Unlock()
	if hit {
		return k.Plan, k.RPM, true
	}
	fetched, err := s.store.GetAPIKey(keyHash)
	if err != nil || fetched == nil {
		return "", 0, false
	}
	s.mu.Lock()
	s.keyCache[keyHash] = *fetched
	s.mu.Unlock()
	return fetched.Plan, fetched.RPM, true
}
func (s *PoWService) cacheKey(k pow.APIKey) {
	s.mu.Lock()
	s.keyCache[k.KeyHash] = k
	s.mu.Unlock()
}

// --- Challenge issuance ---

// clientIP extracts the best-guess client IP (proxy-aware via
// X-Forwarded-For, else RemoteAddr).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tokenBucket is a minimal refill-per-second limiter.
type tokenBucket struct {
	tokens float64
	last   time.Time
	rps    float64
	cap    float64
}

// allow consumes one token; retryAfter reports how long until one is available.
func (b *tokenBucket) allow(now time.Time) (ok bool, retryAfter time.Duration) {
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * b.rps
	if b.tokens > b.cap {
		b.tokens = b.cap
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	deficit := 1 - b.tokens
	return false, time.Duration(deficit / b.rps * float64(time.Second))
}

type dailyCount struct {
	day   string
	n     int
	limit int
}

func (d *dailyCount) allow(now time.Time) bool {
	today := now.Format("2006-01-02")
	if d.day != today {
		d.day = today
		d.n = 0
	}
	if d.n >= d.limit {
		return false
	}
	d.n++
	return true
}

func (s *PoWService) handleChallenge(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()

	// Escalation valve: sustained challenge floods temporarily raise global
	// difficulty for everyone.
	s.mu.Lock()
	if now.Sub(s.escWindowStart) > time.Minute {
		s.escWindowStart = now
		s.escWindowCount = 0
	}
	s.escWindowCount++
	flood := s.cfg.PowChallengePerMin > 0 && s.escWindowCount > s.cfg.PowChallengePerMin*10
	s.mu.Unlock()
	if flood {
		s.diff.Escalate(2, 15*time.Minute)
		s.log.Warn().Str("ip", ip).Msg("Challenge flood detected, escalating global difficulty")
	}

	// Per-IP challenge rate limit (bucket + daily cap).
	s.mu.Lock()
	b, okB := s.chBuckets[ip]
	if !okB {
		perMin := float64(max1(s.cfg.PowChallengePerMin))
		b = &tokenBucket{tokens: perMin, last: now, rps: perMin / 60.0, cap: perMin}
		s.chBuckets[ip] = b
	}
	ok1, retryAfter := b.allow(now)
	d, okD := s.chDaily[ip]
	if !okD {
		d = &dailyCount{limit: max1(s.cfg.PowChallengePerDay)}
		s.chDaily[ip] = d
	}
	ok2 := d.allow(now)
	s.mu.Unlock()

	if !ok1 || !ok2 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
		powErr(w, http.StatusTooManyRequests, "Too many challenge requests; slow down.", "rate_limited")
		return
	}

	plan := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("plan")))
	if plan == "" {
		plan = "basic"
	}
	if !pow.ValidPlan(plan) {
		powErr(w, http.StatusBadRequest, "Unknown plan; use basic, plus, or pro", "invalid_plan")
		return
	}
	algo := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("algo")))
	if algo == "" {
		algo = pow.AlgoSHA256 // browsers solve SHA-256 (WebGPU/WASM friendly)
	}
	if !pow.ValidAlgo(algo) {
		powErr(w, http.StatusBadRequest, "Unsupported algorithm; use blake3 or sha256", "invalid_algo")
		return
	}

	bind := pow.BindingHash(ip, r.UserAgent())
	bonus := s.store.IPBonus(bind)
	difficulty := s.diff.Current(planBits(s.cfg, plan), bonus)

	salt, err := pow.NewSalt(32)
	if err != nil {
		powErr(w, http.StatusInternalServerError, "Failed to mint challenge", "internal_error")
		return
	}

	sc := &pow.StoredChallenge{
		ID:         uuid.NewString(),
		Bind:       bind,
		Plan:       plan,
		Algo:       algo,
		Difficulty: difficulty,
		Salt:       salt,
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(s.cfg.PowChallengeTTL).Unix(),
	}
	if err := s.store.InsertChallenge(sc); err != nil {
		s.log.Error().Err(err).Msg("Failed to store challenge")
		powErr(w, http.StatusInternalServerError, "Failed to mint challenge", "internal_error")
		return
	}

	jsonOut(w, http.StatusOK, map[string]interface{}{
		"challenge":      sc.ToChallenge(),
		"target_seconds": []int{30, 90},
		"warning":        "Solving runs your CPU and GPU at full power. Your device may get hot and battery drain will spike.",
	})
}

// --- Redemption ---

type redeemRequest struct {
	ChallengeID string   `json:"challenge_id"`
	Algo        string   `json:"algo,omitempty"`
	Counters    []string `json:"counters"`
}

func (s *PoWService) handleRedeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		powErr(w, http.StatusBadRequest, "Invalid JSON body", "invalid_json")
		return
	}
	if req.ChallengeID == "" {
		powErr(w, http.StatusBadRequest, "challenge_id is required", "invalid_request")
		return
	}

	ip := clientIP(r)
	sc, err := s.store.GetChallenge(req.ChallengeID)
	if err != nil {
		s.log.Error().Err(err).Msg("Redeem lookup failed")
		powErr(w, http.StatusInternalServerError, "Verification failed", "internal_error")
		return
	}
	if sc == nil {
		powErr(w, http.StatusNotFound, "Unknown challenge", "unknown_challenge")
		return
	}
	now := time.Now()
	if now.Unix() > sc.ExpiresAt {
		powErr(w, http.StatusGone, "Challenge expired", "challenge_expired")
		return
	}
	if sc.Used {
		powErr(w, http.StatusConflict, "Challenge already used", "already_used")
		return
	}

	// Too-fast guard: a real solve at target difficulty cannot complete in
	// half a second. Server-side timing only; no client claims involved.
	issuedAt := time.Unix(sc.IssuedAt, 0)
	if elapsed := now.Sub(issuedAt); elapsed < 500*time.Millisecond {
		s.log.Warn().Str("ip", ip).Dur("elapsed", elapsed).Msg("Suspiciously fast solution rejected")
		powErr(w, http.StatusTooManyRequests, "Solution arrived implausibly fast", "too_fast")
		return
	}

	// Client binding: solutions are only valid from the requesting context.
	if pow.BindingHash(ip, r.UserAgent()) != sc.Bind {
		powErr(w, http.StatusForbidden, "Challenge binding mismatch", "binding_mismatch")
		return
	}

	algo := sc.Algo
	if req.Algo != "" && req.Algo != algo {
		powErr(w, http.StatusBadRequest, "Algorithm does not match challenge", "invalid_algo")
		return
	}

	ch := sc.ToChallenge()
	if _, verr := pow.Verify(ch, req.Counters); verr != nil {
		powErr(w, http.StatusBadRequest, verr.Error(), "invalid_solution")
		return
	}

	// Single-use consume AFTER verification; atomic rows-affected decides.
	consumed, err := s.store.ConsumeChallenge(sc.ID)
	if err != nil {
		s.log.Error().Err(err).Msg("Consume failed")
		powErr(w, http.StatusInternalServerError, "Verification failed", "internal_error")
		return
	}
	if !consumed {
		powErr(w, http.StatusConflict, "Challenge already used", "already_used")
		return
	}

	// Farming penalty: each earned key makes this client's next challenge
	// one bit harder (capped at +8).
	if _, err := s.store.BumpIPBonus(sc.Bind, 8); err != nil {
		s.log.Warn().Err(err).Msg("Failed to bump IP bonus")
	}

	s.diff.RecordSolve(now.Sub(issuedAt))

	rpm := planRPM(s.cfg, sc.Plan)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		powErr(w, http.StatusInternalServerError, "Failed to issue key", "internal_error")
		return
	}
	apiKey := "sk-gw-" + base64.RawURLEncoding.EncodeToString(raw)
	prefix := apiKey[:minStr(len(apiKey), 16)]
	expiresAt := now.Add(s.cfg.PowKeyTTL).Unix()

	k := pow.APIKey{
		KeyHash:   pow.KeyHash(apiKey),
		Prefix:    prefix,
		Plan:      sc.Plan,
		RPM:       rpm,
		CreatedAt: now.Unix(),
		ExpiresAt: expiresAt,
	}
	if err := s.store.InsertAPIKey(k); err != nil {
		s.log.Error().Err(err).Msg("Failed to persist API key")
		powErr(w, http.StatusInternalServerError, "Failed to issue key", "internal_error")
		return
	}
	s.cacheKey(k)

	s.log.Info().Str("prefix", prefix).Str("plan", k.Plan).Int("rpm", rpm).Str("ip", ip).Msg("Issued PoW API key")

	jsonOut(w, http.StatusOK, map[string]interface{}{
		"api_key":    apiKey,
		"plan":       k.Plan,
		"rpm":        k.RPM,
		"prefix":     prefix,
		"created_at": k.CreatedAt,
		"expires_at": k.ExpiresAt,
	})
}

// --- Per-key limits: burst cooldown + plan RPM ---

type rpmBucket struct {
	tokens float64
	last   time.Time
	rpm    float64
}

type burstWin struct {
	t []int64 // unix nanos of recent requests within the last second
}

// checkKeyLimits enforces burst cooldown then plan RPM for an issued key.
// retryAfter > 0 means blocked (code is "key_cooldown" or "rate_limited").
func (s *PoWService) checkKeyLimits(keyHash string, rpm int) (time.Duration, string) {
	if s == nil || rpm <= 0 {
		return 0, ""
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Active cooldown?
	if until, ok := s.cooldowns[keyHash]; ok {
		if now.Before(until) {
			return time.Until(until), "key_cooldown"
		}
		delete(s.cooldowns, keyHash)
	}

	// 2. Burst detection: more than PowBurstRPS requests in any rolling
	//    1-second window trips a punitive cooldown.
	if s.cfg.PowBurstRPS > 0 && s.cfg.PowBurstCooldown > 0 {
		bw, ok := s.burstWin[keyHash]
		if !ok {
			bw = &burstWin{}
			s.burstWin[keyHash] = bw
		}
		cutoff := now.Add(-time.Second).UnixNano()
		kept := bw.t[:0]
		for _, ts := range bw.t {
			if ts > cutoff {
				kept = append(kept, ts)
			}
		}
		bw.t = append(kept, now.UnixNano())
		if len(bw.t) > s.cfg.PowBurstRPS {
			s.cooldowns[keyHash] = now.Add(s.cfg.PowBurstCooldown)
			bw.t = bw.t[:0]
			s.log.Warn().Int("rps_limit", s.cfg.PowBurstRPS).
				Dur("cooldown", s.cfg.PowBurstCooldown).Msg("API key burst limit exceeded; cooling down")
			return s.cfg.PowBurstCooldown, "key_cooldown"
		}
	}

	// 3. Plan RPM token bucket (burst capacity = one minute's worth).
	rb, ok := s.rpmBuckets[keyHash]
	if !ok {
		rb = &rpmBucket{tokens: float64(rpm), last: now, rpm: float64(rpm)}
		s.rpmBuckets[keyHash] = rb
	}
	elapsed := now.Sub(rb.last).Seconds()
	rb.tokens += elapsed * (rb.rpm / 60) // rpm is per MINUTE; elapsed is seconds
	if capF := rb.rpm; rb.tokens > capF {
		rb.tokens = capF
	}
	rb.last = now
	if rb.tokens < 1 {
		deficit := 1 - rb.tokens
		return time.Duration(deficit / rb.rpm * float64(time.Second)), "rate_limited"
	}
	rb.tokens--

	return 0, ""
}

// --- Wiring helpers ---

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

func minStr(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// planBits maps a plan tier to its extra difficulty bits.
func planBits(cfg *config.Config, plan string) int {
	switch plan {
	case "plus":
		return cfg.PowPlan2Difficulty
	case "pro":
		return cfg.PowPlan3Difficulty
	default:
		return cfg.PowPlan1Difficulty
	}
}

// planRPM maps a plan tier to its per-minute rate limit.
func planRPM(cfg *config.Config, plan string) int {
	switch plan {
	case "plus":
		return max1(cfg.PowPlan2RPM)
	case "pro":
		return max1(cfg.PowPlan3RPM)
	default:
		return max1(cfg.PowPlan1RPM)
	}
}

// powErr writes the OpenAI-standard error envelope from package scope.
func powErr(w http.ResponseWriter, status int, message, code string) {
	errObj := map[string]interface{}{
		"message": message,
		"type":    openaiErrorType(status),
		"param":   nil,
		"code":    nil,
	}
	if code != "" {
		errObj["code"] = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{"error": errObj})
}

// jsonOut writes a JSON response from package scope.
func jsonOut(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
