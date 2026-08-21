package metrics

import (
	"strconv"
	"strings"
	"sync"
)

// PriceEntry holds per-1M-token pricing for a model.
type PriceEntry struct {
	InputPer1M  float64
	OutputPer1M float64
	CachedPer1M float64
}

// PricingTable maps model names to their per-1M-token rates.
type PricingTable struct {
	entries map[string]PriceEntry
	mu      sync.RWMutex
}

// NewPricingTable creates a PricingTable from a config string.
// Format: "model_name:input_rate,output_rate,cached_rate;model_name:input_rate,output_rate,cached_rate"
// Rates are per 1M tokens. If cached_rate is omitted, it defaults to input_rate * 0.5.
func NewPricingTable(configStr string) *PricingTable {
	pt := &PricingTable{
		entries: make(map[string]PriceEntry),
	}
	if configStr != "" {
		pt.Parse(configStr)
	}
	return pt
}

// Parse updates the pricing table from a config string.
func (pt *PricingTable) Parse(configStr string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	pt.entries = make(map[string]PriceEntry)
	if configStr == "" {
		return
	}

	for _, part := range strings.Split(configStr, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		model, ratesStr, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}

		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}

		rates := strings.Split(ratesStr, ",")
		if len(rates) < 2 {
			continue
		}

		inputRate, err := strconv.ParseFloat(strings.TrimSpace(rates[0]), 64)
		if err != nil {
			continue
		}

		outputRate, err := strconv.ParseFloat(strings.TrimSpace(rates[1]), 64)
		if err != nil {
			continue
		}

		cachedRate := inputRate * 0.5 // default: 50% of input rate
		if len(rates) >= 3 {
			if cr, err := strconv.ParseFloat(strings.TrimSpace(rates[2]), 64); err == nil {
				cachedRate = cr
			}
		}

		pt.entries[model] = PriceEntry{
			InputPer1M:  inputRate,
			OutputPer1M: outputRate,
			CachedPer1M: cachedRate,
		}
	}
}

// GetEntry returns the pricing entry for a model.
// It tries exact match first, then substring match (model contains key).
func (pt *PricingTable) GetEntry(model string) (PriceEntry, bool) {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	// Exact match
	if entry, ok := pt.entries[model]; ok {
		return entry, true
	}

	// Substring match: find the longest matching key
	var bestKey string
	for key := range pt.entries {
		if strings.Contains(model, key) && len(key) > len(bestKey) {
			bestKey = key
		}
	}
	if bestKey != "" {
		return pt.entries[bestKey], true
	}

	return PriceEntry{}, false
}

// EstimateCost calculates the estimated cost for a given usage.
// Returns cost in dollars (or whatever currency the rates are in).
func (pt *PricingTable) EstimateCost(model string, usage Usage) float64 {
	entry, ok := pt.GetEntry(model)
	if !ok {
		return 0
	}

	cost := (float64(usage.PromptTokens)*entry.InputPer1M +
		float64(usage.CompletionTokens)*entry.OutputPer1M +
		float64(usage.CachedTokens)*entry.CachedPer1M) / 1_000_000

	return cost
}

// EstimateCostFromUpstream calculates the estimated cost for a given upstream usage.
// Returns cost in dollars (or whatever currency the rates are in).
func (pt *PricingTable) EstimateCostFromUpstream(model string, promptTokens, completionTokens, cachedTokens int) float64 {
	entry, ok := pt.GetEntry(model)
	if !ok {
		return 0
	}

	cost := (float64(promptTokens)*entry.InputPer1M +
		float64(completionTokens)*entry.OutputPer1M +
		float64(cachedTokens)*entry.CachedPer1M) / 1_000_000

	return cost
}

// Models returns all configured model names.
func (pt *PricingTable) Models() []string {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	models := make([]string, 0, len(pt.entries))
	for model := range pt.entries {
		models = append(models, model)
	}
	return models
}
