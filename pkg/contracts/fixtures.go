package contracts

import "embed"

// ChatFixtures embeds the model-contract fixtures so the scripted mock provider
// replays the same bytes the contract test pins, instead of keeping a second
// copy that can drift.
//
//go:embed testdata/chat/*.json
var ChatFixtures embed.FS

// ChatFixtureDir is the path prefix inside ChatFixtures.
const ChatFixtureDir = "testdata/chat"
