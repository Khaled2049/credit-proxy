variable "project_id" {
  description = "GCP Project ID"
  type        = string
  default     = "story-6f89f"
}

variable "region" {
  description = "GCP region"
  type        = string
  default     = "us-central1"
}

variable "artifact_registry_repo" {
  description = "Artifact Registry repository name"
  type        = string
  default     = "credit-proxy"
}

variable "gateway_image" {
  description = "Docker image URI for the gateway service"
  type        = string
  validation {
    condition     = length(var.gateway_image) > 0
    error_message = "gateway_image cannot be empty"
  }
}

variable "usage_image" {
  description = "Docker image URI for the usage service"
  type        = string
  validation {
    condition     = length(var.usage_image) > 0
    error_message = "usage_image cannot be empty"
  }
}

variable "llmproxy_image" {
  description = "Docker image URI for the llmproxy service"
  type        = string
  validation {
    condition     = length(var.llmproxy_image) > 0
    error_message = "llmproxy_image cannot be empty"
  }
}

variable "ledger_image" {
  description = "Docker image URI for the ledger service"
  type        = string
  validation {
    condition     = length(var.ledger_image) > 0
    error_message = "ledger_image cannot be empty"
  }
}

variable "gateway_min_instances" {
  description = "Minimum Cloud Run instances for gateway (1 = always warm, avoids GFE routing loss on scale-to-zero)"
  type        = number
  default     = 1
}

variable "min_instances" {
  description = "Minimum Cloud Run instances for internal services (0 = scale to zero)"
  type        = number
  default     = 0
}

variable "max_instances" {
  description = "Maximum Cloud Run instances per service"
  type        = number
  default     = 5
}

variable "gateway_memory" {
  type    = string
  default = "256Mi"
}

variable "usage_memory" {
  type    = string
  default = "256Mi"
}

variable "llmproxy_memory" {
  type    = string
  default = "512Mi"
}

variable "ledger_memory" {
  type    = string
  default = "256Mi"
}

variable "llm_provider" {
  description = "Default LLM provider (gemini | openai | anthropic | ollama | mock)"
  type        = string
  default     = "gemini"
}

variable "gemini_model" {
  description = "Gemini model name used by llmproxy (e.g. gemini-2.5-flash)"
  type        = string
  default     = "gemini-2.5-flash"
}

variable "initial_credits" {
  description = "Free credits granted to new users on first AI call"
  type        = string
  default     = "10000"
}
