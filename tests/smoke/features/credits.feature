Feature: Credit Management
  Users can purchase credits and query their balance

  Background:
    Given a unique test user

  Scenario: Purchasing credits successfully
    When I purchase 2000 credits
    Then the response status should be 200
    And the purchased_credits should be 2000
    And the available_credits should be 2000

  Scenario: Balance reflects purchased credits
    Given I have purchased 1000 credits
    When I check my balance
    Then the response status should be 200
    And the available_credits should be 1000

  Scenario: New user starts with zero balance
    When I check my balance
    Then the response status should be 200
    And the available_credits should be 0

  Scenario: Multiple purchases accumulate
    Given I have purchased 500 credits
    When I purchase 300 credits
    Then the response status should be 200
    And the available_credits should be 800

  Scenario: Cannot purchase zero credits
    When I try to purchase 0 credits
    Then the response status should be 400

  Scenario: Cannot purchase without a user ID
    When I purchase credits without a user ID
    Then the response status should be 400
