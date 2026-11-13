terraform {
  # Ephemeral resources and write-only arguments keep database passwords out
  # of state (database.tf).
  required_version = ">= 1.11.0"

  backend "gcs" {
    bucket = "story-6f89f-tfstate"
    prefix = "credit-proxy"
  }

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.11"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
    postgresql = {
      source  = "cyrilgdn/postgresql"
      version = "~> 1.27"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
