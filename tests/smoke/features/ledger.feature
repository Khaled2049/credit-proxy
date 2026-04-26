Feature: Ledger Events
  All credit activity is recorded in an append-only Postgres audit log.
  Writes are idempotent via idempotency_key.

  Background:
    Given a unique test user
    And a unique idempotency key

  Scenario: Recording a ledger event
    When I record a "credits_reserved" event for 100 credits
    Then the response status should be 200
    And the event type should be "credits_reserved"
    And the event credits should be 100
    And the event should have an ID
    And the event should have a created_at timestamp

  Scenario: Retrieving ledger history
    Given I record a "credits_reserved" event for 200 credits
    And I record a "credits_committed" event for 175 credits with a different idempotency key
    When I retrieve the ledger for my user
    Then the response status should be 200
    And the ledger should contain at least 2 events
    And the most recent event should be "credits_committed"

  Scenario: Ledger writes are idempotent
    When I record a "credits_reserved" event for 100 credits
    Then the response status should be 200
    And I save the event ID

    When I record the same event again
    Then the response status should be 200
    And the event ID should be unchanged

  Scenario: New user has an empty ledger
    When I retrieve the ledger for my user
    Then the response status should be 200
    And the ledger should contain 0 events

  Scenario: Missing required fields are rejected
    When I record an event without an idempotency_key
    Then the response status should be 400
