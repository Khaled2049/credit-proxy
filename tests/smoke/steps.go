//go:build smoke

package smoke

import (
	"fmt"
	"time"

	"github.com/cucumber/godog"
)

// ── Shared setup steps ────────────────────────────────────────────────────────

func (c *smokeCtx) aUniqueTestUser() error {
	c.userID = fmt.Sprintf("smoke-%d", time.Now().UnixNano())
	return nil
}

func (c *smokeCtx) aUniqueTestUserWithNoCredits() error {
	c.userID = fmt.Sprintf("smoke-broke-%d", time.Now().UnixNano())
	return nil
}

func (c *smokeCtx) aUniqueIdempotencyKey() error {
	c.idemKey = fmt.Sprintf("idem-%d", time.Now().UnixNano())
	c.idemKeySufx = 0
	return nil
}

func (c *smokeCtx) iHavePurchasedCredits(amount int) error {
	return c.post(c.usageURL+"/v1/credits/purchase", map[string]any{
		"user_id": c.userID,
		"credits": amount,
	}, nil)
}

// ── Health steps ──────────────────────────────────────────────────────────────

func (c *smokeCtx) iCheckTheHealthOfService(name string) error {
	url := c.serviceURL(name)
	if url == "" {
		return fmt.Errorf("unknown service: %s", name)
	}
	return c.get(url + "/health")
}

// ── Credit steps ──────────────────────────────────────────────────────────────

func (c *smokeCtx) iPurchaseCredits(amount int) error {
	return c.post(c.usageURL+"/v1/credits/purchase", map[string]any{
		"user_id": c.userID,
		"credits": amount,
	}, nil)
}

func (c *smokeCtx) iTryToPurchaseCredits(amount int) error {
	return c.post(c.usageURL+"/v1/credits/purchase", map[string]any{
		"user_id": c.userID,
		"credits": amount,
	}, nil)
}

func (c *smokeCtx) iPurchaseCreditsWithoutUserID() error {
	return c.post(c.usageURL+"/v1/credits/purchase", map[string]any{
		"credits": 100,
	}, nil)
}

func (c *smokeCtx) iCheckMyBalance() error {
	return c.get(fmt.Sprintf("%s/v1/users/%s/balance", c.usageURL, c.userID))
}

// ── Reservation steps ─────────────────────────────────────────────────────────

func (c *smokeCtx) iReserveCreditsWithTTL(amount, ttl int) error {
	if err := c.post(c.usageURL+"/v1/reservations", map[string]any{
		"user_id":           c.userID,
		"estimated_credits": amount,
		"ttl_seconds":       ttl,
	}, nil); err != nil {
		return err
	}
	if id := c.strField("reservation_id"); id != "" {
		c.resID = id
	}
	return nil
}

func (c *smokeCtx) iTryToReserveCredits(amount int) error {
	return c.post(c.usageURL+"/v1/reservations", map[string]any{
		"user_id":           c.userID,
		"estimated_credits": amount,
		"ttl_seconds":       60,
	}, nil)
}

func (c *smokeCtx) iCommitTheReservationWith(actual int) error {
	url := fmt.Sprintf("%s/v1/reservations/%s/commit?user_id=%s", c.usageURL, c.resID, c.userID)
	return c.post(url, map[string]any{"actual_credits": actual}, nil)
}

func (c *smokeCtx) iReleaseTheReservationWithReason(reason string) error {
	url := fmt.Sprintf("%s/v1/reservations/%s/release?user_id=%s", c.usageURL, c.resID, c.userID)
	return c.post(url, map[string]any{"reason": reason}, nil)
}

// ── LLM proxy steps ───────────────────────────────────────────────────────────

func (c *smokeCtx) iCallTheLLMProxyWithPromptAndForceMock(prompt string, forceMockStr string) error {
	return c.post(c.llmURL+"/v1/generate", map[string]any{
		"prompt":            prompt,
		"max_output_tokens": 80,
		"force_mock":        forceMockStr == "true",
	}, nil)
}

func (c *smokeCtx) iCallTheLLMProxyWithoutAPrompt() error {
	return c.post(c.llmURL+"/v1/generate", map[string]any{}, nil)
}

// ── Ledger steps ──────────────────────────────────────────────────────────────

func (c *smokeCtx) iRecordAnEvent(eventType string, credits int) error {
	return c.post(c.ledgerURL+"/v1/events", map[string]any{
		"idempotency_key": c.nextIdemKey(),
		"user_id":         c.userID,
		"event_type":      eventType,
		"credits":         credits,
	}, nil)
}

func (c *smokeCtx) iRecordAnEventWithDifferentKey(eventType string, credits int) error {
	return c.post(c.ledgerURL+"/v1/events", map[string]any{
		"idempotency_key": c.nextIdemKey(),
		"user_id":         c.userID,
		"event_type":      eventType,
		"credits":         credits,
	}, nil)
}

func (c *smokeCtx) iRecordTheSameEventAgain() error {
	// Re-use the current idemKeySufx (don't increment) to replay the last key
	key := fmt.Sprintf("%s:%d", c.idemKey, c.idemKeySufx)
	return c.post(c.ledgerURL+"/v1/events", map[string]any{
		"idempotency_key": key,
		"user_id":         c.userID,
		"event_type":      "credits_reserved",
		"credits":         100,
	}, nil)
}

func (c *smokeCtx) iRecordAnEventWithoutIdempotencyKey() error {
	return c.post(c.ledgerURL+"/v1/events", map[string]any{
		"user_id":    c.userID,
		"event_type": "credits_reserved",
		"credits":    100,
	}, nil)
}

func (c *smokeCtx) iRetrieveTheLedgerForMyUser() error {
	return c.get(fmt.Sprintf("%s/v1/users/%s/ledger", c.ledgerURL, c.userID))
}

func (c *smokeCtx) iSaveTheEventID() error {
	c.savedEventID = c.floatField("id")
	return nil
}

// ── Generate steps ────────────────────────────────────────────────────────────

func (c *smokeCtx) iGenerateTextViaGateway(prompt string) error {
	bal, err := c.balance()
	if err != nil {
		return err
	}
	c.balBefore = bal

	return c.post(c.gatewayURL+"/v1/generate", map[string]any{
		"user_id":           c.userID,
		"prompt":            prompt,
		"max_output_tokens": 80,
		"force_mock":        true,
	}, nil)
}

func (c *smokeCtx) iGenerateTextViaGatewayWithIdempotencyKey(prompt, key string) error {
	return c.post(c.gatewayURL+"/v1/generate", map[string]any{
		"user_id":           c.userID,
		"prompt":            prompt,
		"max_output_tokens": 80,
		"force_mock":        true,
	}, map[string]string{
		"Idempotency-Key": key,
	})
}

func (c *smokeCtx) iGenerateTextViaGatewayWithoutUserID() error {
	return c.post(c.gatewayURL+"/v1/generate", map[string]any{
		"prompt":     "Hello",
		"force_mock": true,
	}, nil)
}

func (c *smokeCtx) iGenerateTextViaGatewayWithoutPrompt() error {
	return c.post(c.gatewayURL+"/v1/generate", map[string]any{
		"user_id":    c.userID,
		"force_mock": true,
	}, nil)
}

// ── Assertion steps ───────────────────────────────────────────────────────────

func (c *smokeCtx) theResponseStatusShouldBe(code int) error {
	if c.lastStatus != code {
		return fmt.Errorf("expected status %d, got %d — body: %v", code, c.lastStatus, c.lastBody)
	}
	return nil
}

func (c *smokeCtx) theResponseShouldContainStatus(status string) error {
	if v := c.strField("status"); v != status {
		return fmt.Errorf("expected status field %q, got %q", status, v)
	}
	return nil
}

func (c *smokeCtx) thePurchasedCreditsShouldBe(want int) error {
	if got := c.floatField("purchased_credits"); got != float64(want) {
		return fmt.Errorf("purchased_credits: got %.0f, want %d", got, want)
	}
	return nil
}

func (c *smokeCtx) theAvailableCreditsShouldBe(want int) error {
	if got := c.floatField("available_credits"); got != float64(want) {
		return fmt.Errorf("available_credits: got %.0f, want %d", got, want)
	}
	return nil
}

func (c *smokeCtx) myBalanceShouldBe(want int) error {
	bal, err := c.balance()
	if err != nil {
		return err
	}
	if bal != float64(want) {
		return fmt.Errorf("balance: got %.0f, want %d", bal, want)
	}
	return nil
}

func (c *smokeCtx) theReservationStatusShouldBe(want string) error {
	if got := c.strField("status"); got != want {
		return fmt.Errorf("reservation status: got %q, want %q", got, want)
	}
	return nil
}

func (c *smokeCtx) theModelShouldBe(want string) error {
	if got := c.strField("model"); got != want {
		return fmt.Errorf("model: got %q, want %q", got, want)
	}
	return nil
}

func (c *smokeCtx) theOutputShouldNotBeEmpty() error {
	if c.strField("output") == "" {
		return fmt.Errorf("output field is empty")
	}
	return nil
}

func (c *smokeCtx) tokenFieldShouldBeGreaterThan0(field string) error {
	usage, ok := c.lastBody["usage"].(map[string]any)
	if !ok {
		return fmt.Errorf("usage field missing or wrong type")
	}
	v, ok := usage[field].(float64)
	if !ok || v <= 0 {
		return fmt.Errorf("%s: expected > 0, got %v", field, usage[field])
	}
	return nil
}

func (c *smokeCtx) theEventTypeShouldBe(want string) error {
	if got := c.strField("event_type"); got != want {
		return fmt.Errorf("event_type: got %q, want %q", got, want)
	}
	return nil
}

func (c *smokeCtx) theEventCreditsShouldBe(want int) error {
	if got := c.floatField("credits"); got != float64(want) {
		return fmt.Errorf("credits: got %.0f, want %d", got, want)
	}
	return nil
}

func (c *smokeCtx) theEventShouldHaveAnID() error {
	if c.floatField("id") == 0 {
		return fmt.Errorf("event id missing or zero")
	}
	return nil
}

func (c *smokeCtx) theEventShouldHaveACreatedAt() error {
	if c.strField("created_at") == "" {
		return fmt.Errorf("created_at field missing or empty")
	}
	return nil
}

func (c *smokeCtx) theLedgerShouldContainAtLeastNEvents(min int) error {
	events, ok := c.lastBody["events"].([]any)
	if !ok {
		return fmt.Errorf("events field missing or wrong type")
	}
	if len(events) < min {
		return fmt.Errorf("expected at least %d events, got %d", min, len(events))
	}
	return nil
}

func (c *smokeCtx) theLedgerShouldContainNEvents(want int) error {
	events, ok := c.lastBody["events"].([]any)
	if !ok {
		return fmt.Errorf("events field missing or wrong type")
	}
	if len(events) != want {
		return fmt.Errorf("expected %d events, got %d", want, len(events))
	}
	return nil
}

func (c *smokeCtx) theMostRecentEventShouldBe(want string) error {
	events, ok := c.lastBody["events"].([]any)
	if !ok || len(events) == 0 {
		return fmt.Errorf("no events in ledger")
	}
	first := events[0].(map[string]any)
	got, _ := first["event_type"].(string)
	if got != want {
		return fmt.Errorf("most recent event_type: got %q, want %q", got, want)
	}
	return nil
}

func (c *smokeCtx) theEventIDShouldBeUnchanged() error {
	if got := c.floatField("id"); got != c.savedEventID {
		return fmt.Errorf("idempotency failed: event ID changed from %.0f to %.0f", c.savedEventID, got)
	}
	return nil
}

func (c *smokeCtx) theResponseContainsAReservationID() error {
	if c.strField("reservation_id") == "" {
		return fmt.Errorf("reservation_id missing from response")
	}
	return nil
}

func (c *smokeCtx) theResponseContainsAnIdempotencyKey() error {
	if c.strField("idempotency_key") == "" {
		return fmt.Errorf("idempotency_key missing from response")
	}
	return nil
}

func (c *smokeCtx) responseFieldShouldBeGreaterThan0(field string) error {
	if v := c.floatField(field); v <= 0 {
		return fmt.Errorf("%s: expected > 0, got %.0f", field, v)
	}
	return nil
}

func (c *smokeCtx) myBalanceIsReducedByActualCreditsUsed() error {
	c.actualSpent = c.floatField("actual_credits")
	bal, err := c.balance()
	if err != nil {
		return err
	}
	expected := c.balBefore - c.actualSpent
	if bal != expected {
		return fmt.Errorf("balance: got %.0f, want %.0f (before=%.0f - actual=%.0f)",
			bal, expected, c.balBefore, c.actualSpent)
	}
	return nil
}

func (c *smokeCtx) theLedgerContainsEventType(eventType string) error {
	if err := c.get(fmt.Sprintf("%s/v1/users/%s/ledger", c.ledgerURL, c.userID)); err != nil {
		return err
	}
	events, ok := c.lastBody["events"].([]any)
	if !ok {
		return fmt.Errorf("events field missing")
	}
	for _, e := range events {
		ev, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if et, _ := ev["event_type"].(string); et == eventType {
			return nil
		}
	}
	return fmt.Errorf("ledger missing %q event", eventType)
}

func (c *smokeCtx) theIdempotencyKeyInTheResponseShouldBe(want string) error {
	if got := c.strField("idempotency_key"); got != want {
		return fmt.Errorf("idempotency_key: got %q, want %q", got, want)
	}
	return nil
}

// ── Step registration ─────────────────────────────────────────────────────────

func InitializeScenario(sc *godog.ScenarioContext) {
	c := newSmokeCtx()

	// Shared setup
	sc.Step(`^a unique test user$`, c.aUniqueTestUser)
	sc.Step(`^a unique test user with no credits$`, c.aUniqueTestUserWithNoCredits)
	sc.Step(`^a unique idempotency key$`, c.aUniqueIdempotencyKey)
	sc.Step(`^I have purchased (\d+) credits$`, c.iHavePurchasedCredits)

	// Health
	sc.Step(`^I check the health of the "([^"]*)" service$`, c.iCheckTheHealthOfService)

	// Credits
	sc.Step(`^I purchase (\d+) credits$`, c.iPurchaseCredits)
	sc.Step(`^I try to purchase (\d+) credits$`, c.iTryToPurchaseCredits)
	sc.Step(`^I purchase credits without a user ID$`, c.iPurchaseCreditsWithoutUserID)
	sc.Step(`^I check my balance$`, c.iCheckMyBalance)

	// Reservations
	sc.Step(`^I reserve (\d+) credits with a (\d+) second TTL$`, c.iReserveCreditsWithTTL)
	sc.Step(`^I try to reserve (\d+) credits$`, c.iTryToReserveCredits)
	sc.Step(`^I commit the reservation with (\d+) actual credits$`, c.iCommitTheReservationWith)
	sc.Step(`^I release the reservation with reason "([^"]*)"$`, c.iReleaseTheReservationWithReason)

	// LLM proxy
	sc.Step(`^I call the LLM proxy with prompt "([^"]*)" and force_mock (true|false)$`, c.iCallTheLLMProxyWithPromptAndForceMock)
	sc.Step(`^I call the LLM proxy without a prompt$`, c.iCallTheLLMProxyWithoutAPrompt)

	// Ledger
	sc.Step(`^I record a "([^"]*)" event for (\d+) credits$`, c.iRecordAnEvent)
	sc.Step(`^I record a "([^"]*)" event for (\d+) credits with a different idempotency key$`, c.iRecordAnEventWithDifferentKey)
	sc.Step(`^I record the same event again$`, c.iRecordTheSameEventAgain)
	sc.Step(`^I record an event without an idempotency_key$`, c.iRecordAnEventWithoutIdempotencyKey)
	sc.Step(`^I retrieve the ledger for my user$`, c.iRetrieveTheLedgerForMyUser)
	sc.Step(`^I save the event ID$`, c.iSaveTheEventID)

	// Generate
	sc.Step(`^I generate text via the gateway with prompt "([^"]*)"$`, c.iGenerateTextViaGateway)
	sc.Step(`^I generate text via the gateway with prompt "([^"]*)" and idempotency key "([^"]*)"$`, c.iGenerateTextViaGatewayWithIdempotencyKey)
	sc.Step(`^I generate text via the gateway without a user ID$`, c.iGenerateTextViaGatewayWithoutUserID)
	sc.Step(`^I generate text via the gateway without a prompt$`, c.iGenerateTextViaGatewayWithoutPrompt)

	// Assertions
	sc.Step(`^the response status should be (\d+)$`, c.theResponseStatusShouldBe)
	sc.Step(`^the response should contain status "([^"]*)"$`, c.theResponseShouldContainStatus)
	sc.Step(`^the purchased_credits should be (\d+)$`, c.thePurchasedCreditsShouldBe)
	sc.Step(`^the available_credits should be (\d+)$`, c.theAvailableCreditsShouldBe)
	sc.Step(`^my balance should be (\d+)$`, c.myBalanceShouldBe)
	sc.Step(`^the reservation status should be "([^"]*)"$`, c.theReservationStatusShouldBe)
	sc.Step(`^the model should be "([^"]*)"$`, c.theModelShouldBe)
	sc.Step(`^the output should not be empty$`, c.theOutputShouldNotBeEmpty)
	sc.Step(`^prompt_tokens should be greater than 0$`, func() error { return c.tokenFieldShouldBeGreaterThan0("prompt_tokens") })
	sc.Step(`^total_tokens should be greater than 0$`, func() error { return c.tokenFieldShouldBeGreaterThan0("total_tokens") })
	sc.Step(`^the event type should be "([^"]*)"$`, c.theEventTypeShouldBe)
	sc.Step(`^the event credits should be (\d+)$`, c.theEventCreditsShouldBe)
	sc.Step(`^the event should have an ID$`, c.theEventShouldHaveAnID)
	sc.Step(`^the event should have a created_at timestamp$`, c.theEventShouldHaveACreatedAt)
	sc.Step(`^the ledger should contain at least (\d+) events$`, c.theLedgerShouldContainAtLeastNEvents)
	sc.Step(`^the ledger should contain (\d+) events$`, c.theLedgerShouldContainNEvents)
	sc.Step(`^the most recent event should be "([^"]*)"$`, c.theMostRecentEventShouldBe)
	sc.Step(`^the event ID should be unchanged$`, c.theEventIDShouldBeUnchanged)
	sc.Step(`^the response contains a reservation ID$`, c.theResponseContainsAReservationID)
	sc.Step(`^the response contains an idempotency key$`, c.theResponseContainsAnIdempotencyKey)
	sc.Step(`^estimated_credits should be greater than 0$`, func() error { return c.responseFieldShouldBeGreaterThan0("estimated_credits") })
	sc.Step(`^actual_credits should be greater than 0$`, func() error { return c.responseFieldShouldBeGreaterThan0("actual_credits") })
	sc.Step(`^my balance is reduced by the actual credits used$`, c.myBalanceIsReducedByActualCreditsUsed)
	sc.Step(`^the ledger contains a "([^"]*)" event$`, c.theLedgerContainsEventType)
	sc.Step(`^the idempotency key in the response should be "([^"]*)"$`, c.theIdempotencyKeyInTheResponseShouldBe)
}
