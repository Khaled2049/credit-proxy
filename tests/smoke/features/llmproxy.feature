Feature: LLM Proxy
  The LLM proxy wraps the Gemini API. In mock mode it returns a canned
  response without making any external API calls.

  Scenario: Generate text in mock mode
    When I call the LLM proxy with prompt "Write a dramatic space opera opening." and force_mock true
    Then the response status should be 200
    And the model should be "mock-gemini"
    And the output should not be empty
    And prompt_tokens should be greater than 0
    And total_tokens should be greater than 0

  Scenario: Token counts reflect actual content lengths
    When I call the LLM proxy with prompt "Hi" and force_mock true
    Then the response status should be 200
    And prompt_tokens should be greater than 0

  Scenario: Missing prompt is rejected
    When I call the LLM proxy without a prompt
    Then the response status should be 400
