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

func EstimatePromptAndMaxCompletion(prompt string, maxCompletion int64) (promptTokens, totalEstimated int64) {
	promptTokens = Estimate(prompt)
	if maxCompletion < 0 {
		maxCompletion = 0
	}
	return promptTokens, promptTokens + maxCompletion
}
