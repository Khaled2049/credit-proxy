package tokens

import "testing"

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
	// "hello world" = 2 words → 2 tokens
	p, tot := EstimatePromptAndMaxCompletion("hello world", 32)
	if p != 2 || tot != 34 {
		t.Fatalf("got (%d,%d), want (2,34)", p, tot)
	}
}
