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

func TestEstimatePromptAndMaxCompletion(t *testing.T) {
	// The result is the TRUE ceiling: promptTokens + maxCompletion (the model
	// cannot emit more than maxCompletion), guaranteeing solvency at reserve time.
	tests := []struct {
		name          string
		prompt        string
		maxCompletion int64
		wantPrompt    int64
		wantTotal     int64
	}{
		{
			name:   "small max adds full max to prompt",
			prompt: "hello world", maxCompletion: 32,
			wantPrompt: 2, wantTotal: 34, // 2 + 32
		},
		{
			name:          "large max is the ceiling",
			prompt:        buildWords(360), // ~360 words → 468 tokens
			maxCompletion: 8192,
			wantPrompt:    468, wantTotal: 8660, // 468 + 8192
		},
		{
			name:   "short prompt still reserves full max",
			prompt: "write me a story", maxCompletion: 1024,
			wantPrompt: 5, wantTotal: 1029, // 5 + 1024
		},
		{
			name:          "long prompt plus max",
			prompt:        buildWords(2308), // ~2308 words → 3000 tokens
			maxCompletion: 8192,
			wantPrompt:    3000, wantTotal: 11192, // 3000 + 8192
		},
		{
			name:   "negative max treated as zero",
			prompt: "hello", maxCompletion: -1,
			wantPrompt: 1, wantTotal: 1, // 1 + 0
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
