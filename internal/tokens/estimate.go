// Package tokens provides a rough token-count estimate for text.
//
// It is a fallback only: token counts are normally authoritative, taken from
// the upstream's own eval stats. Some responses carry none — an OpenAI stream
// without stream_options.include_usage, an error mid-generation, an upstream
// too old to report them — and recording those requests as "0 tokens, $0" makes
// them look free. An estimate keeps the aggregates honest, and every estimated
// row is flagged so it can be told apart from a measured one.
package tokens

import "unicode/utf8"

// charsPerToken is the ratio used for the estimate. Byte-pair encodings of
// English prose land near four characters per token; the estimate is deliberately
// crude and never presented as exact.
const charsPerToken = 4

// Estimate returns an approximate token count for s, rounding up so any
// non-empty text costs at least one token.
func Estimate(s string) int64 {
	if s == "" {
		return 0
	}
	n := utf8.RuneCountInString(s)
	return int64((n + charsPerToken - 1) / charsPerToken)
}
