package main

// These are task identities, not provider or model names. The adapter resolves
// each alias using its server configuration after billing mode is determined.
var routeAliases = map[string]bool{"": true, "prose_draft_v1": true, "prose_rewrite_v1": true, "next_line_v1": true, "voice_analysis_v1": true}

func validRouteAlias(alias string) bool { return routeAliases[alias] }
