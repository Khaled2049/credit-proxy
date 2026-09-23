package tokens

import "strings"

// Estimate approximates token count using a word-based heuristic (~1.3 tokens/word).
// More accurate than chars/4 for English prose while remaining dependency-free.
func Estimate(text string) int64 {
	words := int64(len(strings.Fields(text)))
	if words == 0 {
		return 0
	}
	return max(1, words*13/10)
}

const inputOverheadTokens = 32

func Ceiling(inputBytes int, maxCompletion int64) (promptTokens, totalTokens int64) {
	if inputBytes > 0 {
		promptTokens = int64(inputBytes) + inputOverheadTokens
	}
	if maxCompletion < 0 {
		maxCompletion = 0
	}
	return promptTokens, promptTokens + maxCompletion
}

// ToCredits converts a token count to credits (ceil), with a floor of 1 credit
// for any non-zero usage. Ceil preserves monotonicity — reserved credits are
// always >= actual credits whenever reserved tokens >= actual tokens — so the
// commit refund path stays correct.
func ToCredits(tokenCount, tokensPerCredit int64) int64 {
	if tokensPerCredit < 1 {
		tokensPerCredit = 1
	}
	if tokenCount <= 0 {
		return 0
	}
	return max(1, (tokenCount+tokensPerCredit-1)/tokensPerCredit)
}
