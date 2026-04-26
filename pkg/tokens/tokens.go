package tokens

// Estimate approximates token count with a simple heuristic for demos.
// This keeps local behavior deterministic without external tokenizers.
func Estimate(text string) int64 {
	if text == "" {
		return 0
	}
	chars := len(text)
	toks := chars / 4
	if chars%4 != 0 {
		toks++
	}
	if toks < 1 {
		toks = 1
	}
	return int64(toks)
}

func EstimatePromptAndMaxCompletion(prompt string, maxCompletion int64) (promptTokens, totalEstimated int64) {
	promptTokens = Estimate(prompt)
	if maxCompletion < 0 {
		maxCompletion = 0
	}
	return promptTokens, promptTokens + maxCompletion
}
