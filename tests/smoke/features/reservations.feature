Feature: Credit Reservations
  Credits are atomically reserved before LLM calls and settled afterward.
  The commit step reconciles estimated vs actual token cost.

  Background:
    Given a unique test user
    And I have purchased 500 credits

  Scenario: Reserve then commit with a refund
    When I reserve 100 credits with a 60 second TTL
    Then the response status should be 200
    And the reservation status should be "reserved"
    And my balance should be 400

    When I commit the reservation with 80 actual credits
    Then the response status should be 200
    And the reservation status should be "committed"
    And my balance should be 420

  Scenario: Reserve then commit with an extra charge
    When I reserve 50 credits with a 60 second TTL
    Then the response status should be 200
    And my balance should be 450

    When I commit the reservation with 70 actual credits
    Then the response status should be 200
    And the reservation status should be "committed"
    And my balance should be 430

  Scenario: Reserve then release restores balance
    When I reserve 150 credits with a 60 second TTL
    Then the response status should be 200
    And my balance should be 350

    When I release the reservation with reason "llm_failure"
    Then the response status should be 200
    And the reservation status should be "released"
    And my balance should be 500

  Scenario: Cannot reserve more than the available balance
    When I try to reserve 600 credits
    Then the response status should be 402

  Scenario: Committing a reservation is idempotent
    When I reserve 100 credits with a 60 second TTL
    And I commit the reservation with 80 actual credits
    Then the response status should be 200

    When I commit the reservation with 80 actual credits
    Then the response status should be 200

  Scenario: Cannot release an already committed reservation
    When I reserve 100 credits with a 60 second TTL
    And I commit the reservation with 100 actual credits
    When I release the reservation with reason "test"
    Then the response status should be 409
