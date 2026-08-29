package pricing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCost_PerModelRate(t *testing.T) {
	tbl := &Table{
		Currency: "USD",
		Default:  ModelRate{PromptPer1K: 1, CompletionPer1K: 2},
		Models: map[string]ModelRate{
			"gpt-4o": {PromptPer1K: 5, CompletionPer1K: 15},
		},
	}
	if got := tbl.Cost("gpt-4o", 1000, 1000); got != 20 {
		t.Errorf("gpt-4o cost = %v, want 20", got)
	}
	// Unlisted model falls back to Default.
	if got := tbl.Cost("mystery", 2000, 1000); got != 4 { // 2*1 + 1*2
		t.Errorf("default cost = %v, want 4", got)
	}
}

func TestCost_NilTableIsZero(t *testing.T) {
	var tbl *Table
	if got := tbl.Cost("x", 100, 100); got != 0 {
		t.Errorf("nil table cost = %v, want 0", got)
	}
}

func TestLoad_EmptyPathYieldsEmpty(t *testing.T) {
	tbl, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if tbl.Currency != "USD" || len(tbl.Models) != 0 {
		t.Errorf("expected empty USD table, got %+v", tbl)
	}
}

func TestLoad_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.json")
	if err := os.WriteFile(path, []byte(`{"currency":"EUR","models":{"m":{"prompt_per_1k":3,"completion_per_1k":6}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tbl, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tbl.Currency != "EUR" {
		t.Errorf("currency = %q, want EUR", tbl.Currency)
	}
	if got := tbl.Cost("m", 1000, 1000); got != 9 {
		t.Errorf("cost = %v, want 9", got)
	}
}

func TestCost_FallsBackToTheUntaggedRate(t *testing.T) {
	table := &Table{
		Currency: "USD",
		Models:   map[string]ModelRate{"llama3": {PromptPer1K: 1, CompletionPer1K: 2}},
	}
	// Normalization records "llama3:latest"; the table still prices it.
	if got := table.Cost("llama3:latest", 1000, 1000); got != 3 {
		t.Fatalf("Cost(llama3:latest) = %v, want 3", got)
	}
}

func TestCost_ExactTagBeatsTheUntaggedRate(t *testing.T) {
	table := &Table{
		Currency: "USD",
		Models: map[string]ModelRate{
			"llama3":     {PromptPer1K: 1, CompletionPer1K: 1},
			"llama3:70b": {PromptPer1K: 10, CompletionPer1K: 10},
		},
	}
	if got := table.Cost("llama3:70b", 1000, 0); got != 10 {
		t.Fatalf("Cost(llama3:70b) = %v, want the exact rate 10", got)
	}
}

func TestCost_RegistryPortIsNotATag(t *testing.T) {
	table := &Table{
		Currency: "USD",
		Models:   map[string]ModelRate{"registry:5000": {PromptPer1K: 99}},
	}
	// "registry:5000/ns/model" must not be priced as the host "registry:5000".
	if got := table.Cost("registry:5000/ns/model", 1000, 0); got != 0 {
		t.Fatalf("Cost(registry path) = %v, want 0", got)
	}
}
