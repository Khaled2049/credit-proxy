package tokens

import (
	"strings"
	"testing"
)

func TestEstimate(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"hello", 1},              // 1 word → 1*13/10 = 1
		{"hello world", 2},        // 2 words → 2*13/10 = 2
		{"one two three four", 5}, // 4 words → 4*13/10 = 5
	}
	for _, tt := range tests {
		if got := Estimate(tt.in); got != tt.want {
			t.Fatalf("Estimate(%q)=%d want %d", tt.in, got, tt.want)
		}
	}
}

func TestCeiling(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		maxCompletion int64
		wantPrompt    int64
		wantTotal     int64
	}{
		{name: "empty input reserves only the completion", input: "", maxCompletion: 100, wantPrompt: 0, wantTotal: 100},
		{name: "short prose", input: "hello world", maxCompletion: 32, wantPrompt: 43, wantTotal: 75},
		{name: "long input without spaces counts every byte", input: strings.Repeat("x", 60000), maxCompletion: 1000, wantPrompt: 60032, wantTotal: 61032},
		{name: "digits count one token each", input: strings.Repeat("7", 5000), maxCompletion: 0, wantPrompt: 5032, wantTotal: 5032},
		{name: "multibyte text counts bytes not runes", input: "日本語", maxCompletion: 0, wantPrompt: 41, wantTotal: 41},
		{name: "negative max treated as zero", input: "hello", maxCompletion: -1, wantPrompt: 37, wantTotal: 37},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, tot := Ceiling(len(tt.input), tt.maxCompletion)
			if p != tt.wantPrompt || tot != tt.wantTotal {
				t.Fatalf("prompt=%d total=%d, want prompt=%d total=%d", p, tot, tt.wantPrompt, tt.wantTotal)
			}
		})
	}
}

func TestCeilingBoundsWordHeuristic(t *testing.T) {
	for _, input := range []string{"hello", buildWords(2308), strings.Repeat("a", 10000), "a b c d e f g"} {
		if p, _ := Ceiling(len(input), 0); p < Estimate(input) {
			t.Fatalf("ceiling %d below heuristic %d for %q", p, Estimate(input), input[:min(len(input), 20)])
		}
	}
}

func TestToCredits(t *testing.T) {
	tests := []struct {
		name            string
		tokenCount      int64
		tokensPerCredit int64
		want            int64
	}{
		{"zero tokens is zero credits", 0, 100, 0},
		{"negative tokens is zero credits", -50, 100, 0},
		{"exact multiple", 1000, 100, 10},
		{"rounds up (ceil)", 1001, 100, 11},
		{"tiny usage floored at 1 credit", 1, 100, 1},
		{"just under one credit floored at 1", 99, 100, 1},
		{"ratio of 1 is passthrough", 4500, 1, 4500},
		{"invalid ratio treated as 1", 4500, 0, 4500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToCredits(tt.tokenCount, tt.tokensPerCredit); got != tt.want {
				t.Errorf("ToCredits(%d, %d) = %d, want %d", tt.tokenCount, tt.tokensPerCredit, got, tt.want)
			}
		})
	}
}

// buildWords returns a string of n space-separated "word" tokens.
func buildWords(n int) string {
	words := make([]string, n)
	for i := range words {
		words[i] = "word"
	}
	return strings.Join(words, " ")
}
