// Package pricing computes a per-request cost estimate from token counts using
// a configurable, per-model rate table.
//
// Local Ollama inference is free, so the default rates are zero; the feature is
// useful for "what would this have cost on a hosted API" comparisons and for
// internal chargeback. Costs are computed once, at request time, and stored on
// the row — so historical rows reflect the price in effect when they ran.
package pricing

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ModelRate is the price per 1,000 tokens for one model.
type ModelRate struct {
	PromptPer1K     float64 `json:"prompt_per_1k"`
	CompletionPer1K float64 `json:"completion_per_1k"`
}

// Table maps model names to rates, with a fallback Default for unlisted models.
type Table struct {
	Currency string               `json:"currency"`
	Default  ModelRate            `json:"default"`
	Models   map[string]ModelRate `json:"models"`
}

// Empty returns a zero-cost table (currency USD, no per-model rates).
func Empty() *Table {
	return &Table{Currency: "USD", Models: map[string]ModelRate{}}
}

// Load reads a pricing table from a JSON file. An empty path yields Empty().
func Load(path string) (*Table, error) {
	if path == "" {
		return Empty(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pricing file %q: %w", path, err)
	}
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse pricing file %q: %w", path, err)
	}
	if t.Currency == "" {
		t.Currency = "USD"
	}
	if t.Models == nil {
		t.Models = map[string]ModelRate{}
	}
	return &t, nil
}

// rateFor returns the rate for model, falling back first to the same model
// without its tag and then to Default. The untagged fallback is what lets a
// table written as "llama3" keep pricing requests recorded as "llama3:latest",
// while a table that prices "llama3:70b" separately still wins on exact match.
func (t *Table) rateFor(model string) ModelRate {
	if r, ok := t.Models[model]; ok {
		return r
	}
	if base, ok := untagged(model); ok {
		if r, ok := t.Models[base]; ok {
			return r
		}
	}
	return t.Default
}

// untagged strips a trailing ":tag" from a model name, ignoring a colon that
// belongs to a registry host and port ("registry:5000/ns/model").
func untagged(model string) (string, bool) {
	slash := strings.LastIndex(model, "/")
	colon := strings.LastIndex(model, ":")
	if colon <= slash {
		return "", false
	}
	return model[:colon], true
}

// Cost returns the estimated cost for the given token counts under model's rate.
func (t *Table) Cost(model string, promptTokens, completionTokens int64) float64 {
	if t == nil {
		return 0
	}
	r := t.rateFor(model)
	return float64(promptTokens)/1000.0*r.PromptPer1K +
		float64(completionTokens)/1000.0*r.CompletionPer1K
}
