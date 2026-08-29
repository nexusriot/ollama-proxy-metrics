package modelname

import "testing"

func TestNormalize_AddsLatestTag(t *testing.T) {
	if got := Normalize("llama3", nil); got != "llama3:latest" {
		t.Fatalf("Normalize(llama3) = %q, want llama3:latest", got)
	}
}

func TestNormalize_KeepsExistingTag(t *testing.T) {
	for _, in := range []string{"llama3:8b", "llama3:latest", "registry:5000/ns/m:v1"} {
		if got := Normalize(in, nil); got != in {
			t.Errorf("Normalize(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestNormalize_RegistryPortIsNotATag(t *testing.T) {
	got := Normalize("registry:5000/ns/model", nil)
	if got != "registry:5000/ns/model:latest" {
		t.Fatalf("Normalize(registry with port) = %q, want the :latest suffix", got)
	}
}

func TestNormalize_PreservesEmptyAndUnknown(t *testing.T) {
	if got := Normalize("", nil); got != "" {
		t.Errorf("Normalize(\"\") = %q, want empty", got)
	}
	if got := Normalize(Unknown, nil); got != Unknown {
		t.Errorf("Normalize(unknown) = %q, want unknown", got)
	}
}

func TestNormalize_AppliesAliasThenTag(t *testing.T) {
	aliases := map[string]string{"fast": "qwen2.5:0.5b", "big": "llama3"}
	if got := Normalize("fast", aliases); got != "qwen2.5:0.5b" {
		t.Errorf("aliased model = %q, want qwen2.5:0.5b", got)
	}
	if got := Normalize("big", aliases); got != "llama3:latest" {
		t.Errorf("aliased untagged model = %q, want llama3:latest", got)
	}
}

func TestParseAliases(t *testing.T) {
	got, err := ParseAliases(" fast=qwen2.5:0.5b , big=llama3:70b ")
	if err != nil {
		t.Fatalf("ParseAliases: %v", err)
	}
	if got["fast"] != "qwen2.5:0.5b" || got["big"] != "llama3:70b" {
		t.Fatalf("ParseAliases = %v", got)
	}
}

func TestParseAliases_EmptyAndInvalid(t *testing.T) {
	got, err := ParseAliases("")
	if err != nil || got != nil {
		t.Fatalf("ParseAliases(\"\") = %v, %v; want nil, nil", got, err)
	}
	if _, err := ParseAliases("nope"); err == nil {
		t.Fatal("ParseAliases(\"nope\") = nil error, want an error")
	}
}
