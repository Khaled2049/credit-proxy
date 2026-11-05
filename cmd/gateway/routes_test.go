package main

import "testing"

func TestRouteAliasAllowlist(t *testing.T) {
	for _, alias := range []string{"", "prose_draft_v1", "prose_rewrite_v1", "next_line_v1", "voice_analysis_v1"} {
		if !validRouteAlias(alias) {
			t.Errorf("known route %q rejected", alias)
		}
	}
	for _, alias := range []string{"gemini/gemini-2.5-pro", "arbitrary", "NEXT_LINE_V1"} {
		if validRouteAlias(alias) {
			t.Errorf("unknown route %q accepted", alias)
		}
	}
}
