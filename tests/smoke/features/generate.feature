Feature: End-to-End Credit-Metered Generation
  The gateway orchestrates: reserve credits → call LLM → commit actual cost.
  On failure the reservation is released so credits are never lost.

  Background:
    Given a unique test user
    And I have purchased 2000 credits

  Scenario: Successful generation debits actual credits
    When I generate text via the gateway with prompt "Write a dramatic opening scene for a space opera."
    Then the response status should be 200
    And the response contains a reservation ID
    And the response contains an idempotency key
    And estimated_credits should be greater than 0
    And actual_credits should be greater than 0
    And my balance is reduced by the actual credits used
    And the ledger contains a "credits_reserved" event
    And the ledger contains a "credits_committed" event

  Scenario: Generation with a client-supplied idempotency key
    When I generate text via the gateway with prompt "Hello world" and idempotency key "e2e-smoke-key-123"
    Then the response status should be 200
    And the idempotency key in the response should be "e2e-smoke-key-123"

  Scenario: Generation is rejected without sufficient credits
    Given a unique test user with no credits
    When I generate text via the gateway with prompt "Hello"
    Then the response status should be 402

  Scenario: Generation requires a user ID
    When I generate text via the gateway without a user ID
    Then the response status should be 400

  Scenario: Generation requires a prompt
    When I generate text via the gateway without a prompt
    Then the response status should be 400
