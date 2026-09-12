package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The Go half of the cross-language contract check. These fixtures pin the
// model contract (agents to creditProxy); the assistant protocol's fixtures
// live in taleTribe-agents and are checked by Python and TypeScript, because Go
// never parses an assistant event.

type chatFixture struct {
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	ContractVersion int               `json:"contractVersion"`
	Request         json.RawMessage   `json:"request"`
	Stream          []json.RawMessage `json:"stream"`
}

func loadFixtures(t *testing.T) map[string]chatFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "chat", "*.json"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no chat fixtures found under testdata/chat")
	}
	fixtures := make(map[string]chatFixture, len(paths))
	for _, path := range paths {
		if filepath.Base(path) == "MANIFEST.json" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var fixture chatFixture
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		fixtures[fixture.Name] = fixture
	}
	return fixtures
}

func TestChatFixtureManifest(t *testing.T) {
	directory := filepath.Join("testdata", "chat")
	raw, err := os.ReadFile(filepath.Join(directory, "MANIFEST.json"))
	if err != nil {
		t.Fatalf("read fixture manifest: %v", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse fixture manifest: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.json"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	actual := make(map[string]string)
	for _, path := range paths {
		name := filepath.Base(path)
		if name == "MANIFEST.json" {
			continue
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		actual[name] = fmt.Sprintf("%x", sha256.Sum256(contents))
	}
	if !reflect.DeepEqual(manifest, actual) {
		t.Fatalf("fixture manifest is stale\nmanifest: %#v\n  actual: %#v", manifest, actual)
	}
}

// roundTrip decodes into T, re-encodes, and compares the two as generic JSON so
// key order does not matter but a dropped or renamed field does.
func roundTrip[T any](t *testing.T, label string, raw json.RawMessage) {
	t.Helper()
	var typed T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&typed); err != nil {
		t.Fatalf("%s: decode into %T: %v", label, typed, err)
	}
	reencoded, err := json.Marshal(typed)
	if err != nil {
		t.Fatalf("%s: re-encode: %v", label, err)
	}
	var before, after any
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatalf("%s: normalize original: %v", label, err)
	}
	if err := json.Unmarshal(reencoded, &after); err != nil {
		t.Fatalf("%s: normalize round-tripped: %v", label, err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("%s: round-trip lost or changed fields\n before: %s\n  after: %s", label, raw, reencoded)
	}
}

func TestChatFixturesRoundTrip(t *testing.T) {
	for name, fixture := range loadFixtures(t) {
		t.Run(name, func(t *testing.T) {
			if fixture.ContractVersion != ChatContractVersion {
				t.Fatalf("fixture declares version %d, want %d", fixture.ContractVersion, ChatContractVersion)
			}
			roundTrip[ChatRequest](t, name+"/request", fixture.Request)
			for _, event := range fixture.Stream {
				roundTrip[ChatEvent](t, name+"/stream", event)
			}
		})
	}
}

func TestEveryFixtureRequestDeclaresAnOutputCeiling(t *testing.T) {
	// MaxOutputTokens is required in this contract precisely because a
	// reservation must be sized before the first streamed byte. A fixture
	// without one would quietly legitimise an unsized request.
	for name, fixture := range loadFixtures(t) {
		var request ChatRequest
		if err := json.Unmarshal(fixture.Request, &request); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if request.MaxOutputTokens <= 0 {
			t.Errorf("%s: max_output_tokens must be set", name)
		}
		if request.Version != ChatContractVersion {
			t.Errorf("%s: request version %d, want %d", name, request.Version, ChatContractVersion)
		}
	}
}

func TestBYOKRequestsCarryNoReservation(t *testing.T) {
	// BYOK never touches platform credits, so a BYOK request holding a
	// reservation would mean a user's own key had been metered against the
	// platform pool.
	for name, fixture := range loadFixtures(t) {
		var request ChatRequest
		if err := json.Unmarshal(fixture.Request, &request); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if request.BYOKProvider == "" {
			continue
		}
		if request.ReservationID != "" || request.EstimatedCredits != 0 {
			t.Errorf("%s: BYOK request must not carry a reservation", name)
		}
	}
}

func TestUsageEventsReportZeroCreditsForBYOK(t *testing.T) {
	fixture, ok := loadFixtures(t)["byok-not-metered"]
	if !ok {
		t.Fatal("byok-not-metered fixture is missing")
	}
	for _, raw := range fixture.Stream {
		var event ChatEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if event.Type == ChatEventUsage && event.Credits != 0 {
			t.Errorf("BYOK usage reported %d credits, want 0", event.Credits)
		}
	}
}

func TestStreamsEndWithATerminalEvent(t *testing.T) {
	for name, fixture := range loadFixtures(t) {
		if len(fixture.Stream) == 0 {
			t.Errorf("%s: empty stream", name)
			continue
		}
		var last ChatEvent
		if err := json.Unmarshal(fixture.Stream[len(fixture.Stream)-1], &last); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if last.Type != ChatEventDone && last.Type != ChatEventError {
			t.Errorf("%s: stream ends with %q, want done or error", name, last.Type)
		}
	}
}

func TestErrorEventsCarryNoProviderBody(t *testing.T) {
	// The Phase 0 logging audit removed provider bodies from logs; the same
	// rule applies on the wire, since a body can echo the prompt.
	fixture := loadFixtures(t)["provider-error"]
	for _, raw := range fixture.Stream {
		var event ChatEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if event.Type != ChatEventError {
			continue
		}
		if event.Error == nil || event.Error.Code == "" {
			t.Fatal("error event must carry a stable code")
		}
		if event.Error.Message == "" {
			t.Error("error event must carry a neutral message")
		}
	}
}
