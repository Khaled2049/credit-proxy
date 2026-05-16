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
		{"hello", 1},           // 1 word → 1*13/10 = 1
		{"hello world", 2},     // 2 words → 2*13/10 = 2
		{"one two three four", 5}, // 4 words → 4*13/10 = 5
	}
	for _, tt := range tests {
		if got := Estimate(tt.in); got != tt.want {
			t.Fatalf("Estimate(%q)=%d want %d", tt.in, got, tt.want)
		}
	}
}

func TestEstimatePromptAndMaxCompletion(t *testing.T) {
	tests := []struct {
		name          string
		prompt        string
		maxCompletion int64
		wantPrompt    int64
		wantTotal     int64
	}{
		{
			// max < 256: completion capped at max, not floored at 256
			name: "small max caps completion",
			prompt: "hello world", maxCompletion: 32,
			wantPrompt: 2, wantTotal: 34, // 2 + min(32, max(256,2)) = 2+32 = 34
		},
		{
			// typical request: large max, moderate prompt — completion = promptTokens
			// old behaviour: 468 + 8192 = 8660; new: 468 + 468 = 936
			name:          "large max uses prompt-proportional estimate",
			prompt:        buildWords(360), // ~360 words → ~468 tokens
			maxCompletion: 8192,
			wantPrompt:    468, wantTotal: 936, // 468 + min(8192, max(256,468)) = 468+468
		},
		{
			// very short prompt: floor kicks in
			// "write me a story" = 4 words → 4*13/10 = 5 tokens
			name: "short prompt uses 256 floor",
			prompt: "write me a story", maxCompletion: 8192,
			wantPrompt: 5, wantTotal: 261, // 5 + min(8192, max(256,5)) = 5+256
		},
		{
			// long prompt: completion = promptTokens (proportional, under max)
			name:          "long prompt scales completion",
			prompt:        buildWords(2308), // ~2308 words → ~3000 tokens
			maxCompletion: 8192,
			wantPrompt:    3000, wantTotal: 6000,
		},
		{
			name: "negative max treated as zero",
			prompt: "hello", maxCompletion: -1,
			wantPrompt: 1, wantTotal: 1, // 1 + min(0, max(256,1)) = 1+0
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, tot := EstimatePromptAndMaxCompletion(tt.prompt, tt.maxCompletion)
			if p != tt.wantPrompt || tot != tt.wantTotal {
				t.Fatalf("prompt=%d total=%d, want prompt=%d total=%d", p, tot, tt.wantPrompt, tt.wantTotal)
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
