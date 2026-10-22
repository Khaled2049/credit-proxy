package main

// usageFeatures is the closed set of feature names the ledger records. KEEP IN
// SYNC with USAGE_FEATURES in taleTribe-agents agents/storyAgent/llm_provider.py.
var usageFeatures = map[string]bool{
	"next_line":            true,
	"enhance":              true,
	"brainstorm":           true,
	"story_choices":        true,
	"wizard_enhance":       true,
	"summarize":            true,
	"assistant":            true,
	"assistant_specialist": true,
	"assistant_drafting":   true,
	"other":                true,
}

// usageFeature folds an unknown or missing tag into "other" rather than
// rejecting the request: the tag is reporting, never a reason to refuse work.
func usageFeature(raw string) string {
	if usageFeatures[raw] {
		return raw
	}
	return "other"
}
