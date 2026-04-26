package tokens

import "testing"

func TestEstimate(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"abcd", 1},
		{"abcde", 2},
		{"hello world", 3},
	}
	for _, tt := range tests {
		if got := Estimate(tt.in); got != tt.want {
			t.Fatalf("Estimate(%q)=%d want %d", tt.in, got, tt.want)
		}
	}
}

func TestEstimatePromptAndMaxCompletion(t *testing.T) {
	p, tot := EstimatePromptAndMaxCompletion("abcd", 32)
	if p != 1 || tot != 33 {
		t.Fatalf("got (%d,%d), want (1,33)", p, tot)
	}
}
