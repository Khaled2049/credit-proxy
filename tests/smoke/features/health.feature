Feature: Service Health
  All four services expose a /health endpoint that confirms readiness

  Scenario Outline: Each service responds to health checks
    When I check the health of the "<service>" service
    Then the response status should be 200
    And the response should contain status "ok"

    Examples:
      | service  |
      | gateway  |
      | usage    |
      | llmproxy |
      | ledger   |
