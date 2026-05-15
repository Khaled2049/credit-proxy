# ── GCP APIs ─────────────────────────────────────────────────────────────────

resource "google_project_service" "run" {
  service            = "run.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "secretmanager" {
  service            = "secretmanager.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "artifactregistry" {
  service            = "artifactregistry.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "iam" {
  service            = "iam.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "cloudresourcemanager" {
  service            = "cloudresourcemanager.googleapis.com"
  disable_on_destroy = false
}

# ── Pre-created resources (data sources) ─────────────────────────────────────

data "google_service_account" "run_sa" {
  account_id = "credit-proxy-run"
  project    = var.project_id
}

data "google_artifact_registry_repository" "docker_repo" {
  location      = var.region
  repository_id = var.artifact_registry_repo
  project       = var.project_id
}

# ── Secret Manager — API key shells created by workflow ──────────────────────

data "google_secret_manager_secret" "gemini_api_key" {
  secret_id = "creditproxy-gemini-api-key"
  project   = var.project_id
}

data "google_secret_manager_secret" "openai_api_key" {
  secret_id = "creditproxy-openai-api-key"
  project   = var.project_id
}

data "google_secret_manager_secret" "anthropic_api_key" {
  secret_id = "creditproxy-anthropic-api-key"
  project   = var.project_id
}

data "google_secret_manager_secret" "upstash_redis_url" {
  secret_id = "creditproxy-upstash-redis-url"
  project   = var.project_id
}

# ── Secret Manager — Postgres DSN managed by workflow ────────────────────────
# Shell and version are created by deploy.yml (same pattern as API keys).
# Terraform reads it as a data source to apply IAM — never sees the raw value.

data "google_secret_manager_secret" "postgres_dsn" {
  secret_id = "creditproxy-postgres-dsn"
  project   = var.project_id
}

# ── IAM — Cloud Run SA secret access ─────────────────────────────────────────

locals {
  secret_ids = {
    gemini       = data.google_secret_manager_secret.gemini_api_key.secret_id
    openai       = data.google_secret_manager_secret.openai_api_key.secret_id
    anthropic    = data.google_secret_manager_secret.anthropic_api_key.secret_id
    upstash      = data.google_secret_manager_secret.upstash_redis_url.secret_id
    postgres_dsn = data.google_secret_manager_secret.postgres_dsn.secret_id
  }
}

resource "google_secret_manager_secret_iam_member" "run_sa_secret_access" {
  for_each  = local.secret_ids
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${data.google_service_account.run_sa.email}"
  project   = var.project_id
}

# ── Cloud Run — shared locals ─────────────────────────────────────────────────

locals {
  sa_email = data.google_service_account.run_sa.email
}

# ── Cloud Run — usage ─────────────────────────────────────────────────────────

resource "google_cloud_run_v2_service" "usage" {
  name     = "credit-proxy-usage"
  location = var.region
  project  = var.project_id
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = local.sa_email

    scaling {
      min_instance_count = var.min_instances
      max_instance_count = var.max_instances
    }

    containers {
      image = var.usage_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = var.usage_memory
        }
        cpu_idle          = true
        startup_cpu_boost = true
      }

      env {
        name  = "USAGE_ADDR"
        value = ":8080"
      }

      env {
        name  = "INITIAL_CREDITS"
        value = var.initial_credits
      }

      env {
        name = "REDIS_URL"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.upstash_redis_url.secret_id
            version = "latest"
          }
        }
      }

      # /health returns static {"status":"ok"} — safe for liveness.
      # Do NOT add Redis connectivity to /health; a Redis blip would restart healthy pods.
      startup_probe {
        http_get {
          path = "/health"
          port = 8080
        }
        initial_delay_seconds = 5
        timeout_seconds       = 3
        period_seconds        = 10
        failure_threshold     = 8
      }

      liveness_probe {
        http_get {
          path = "/health"
          port = 8080
        }
        initial_delay_seconds = 30
        timeout_seconds       = 3
        period_seconds        = 30
        failure_threshold     = 3
      }
    }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
  }

  depends_on = [
    google_project_service.run,
    google_secret_manager_secret_iam_member.run_sa_secret_access,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "usage_invoker" {
  name     = google_cloud_run_v2_service.usage.name
  location = var.region
  project  = var.project_id
  role     = "roles/run.invoker"
  member   = "serviceAccount:${local.sa_email}"
}

# ── Cloud Run — llmproxy ──────────────────────────────────────────────────────

resource "google_cloud_run_v2_service" "llmproxy" {
  name     = "credit-proxy-llmproxy"
  location = var.region
  project  = var.project_id
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = local.sa_email

    scaling {
      min_instance_count = var.min_instances
      max_instance_count = var.max_instances
    }

    # LLM calls are slow; low concurrency prevents head-of-line blocking.
    max_instance_request_concurrency = 10

    containers {
      image = var.llmproxy_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = var.llmproxy_memory
        }
        cpu_idle          = true
        startup_cpu_boost = true
      }

      env {
        name  = "LLMPROXY_ADDR"
        value = ":8080"
      }

      env {
        name  = "LLM_PROVIDER"
        value = var.llm_provider
      }

      env {
        name  = "GEMINI_MODEL"
        value = var.gemini_model
      }

      env {
        name  = "OPENAI_MODEL"
        value = "gpt-4o-mini"
      }

      env {
        name  = "ANTHROPIC_MODEL"
        value = "claude-sonnet-4-6"
      }

      env {
        name = "GEMINI_API_KEY"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.gemini_api_key.secret_id
            version = "latest"
          }
        }
      }

      env {
        name = "OPENAI_API_KEY"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.openai_api_key.secret_id
            version = "latest"
          }
        }
      }

      env {
        name = "ANTHROPIC_API_KEY"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.anthropic_api_key.secret_id
            version = "latest"
          }
        }
      }

      startup_probe {
        http_get {
          path = "/health"
          port = 8080
        }
        initial_delay_seconds = 5
        timeout_seconds       = 3
        period_seconds        = 10
        failure_threshold     = 8
      }

      liveness_probe {
        http_get {
          path = "/health"
          port = 8080
        }
        initial_delay_seconds = 30
        timeout_seconds       = 3
        period_seconds        = 30
        failure_threshold     = 3
      }
    }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
  }

  depends_on = [
    google_project_service.run,
    google_secret_manager_secret_iam_member.run_sa_secret_access,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "llmproxy_invoker" {
  name     = google_cloud_run_v2_service.llmproxy.name
  location = var.region
  project  = var.project_id
  role     = "roles/run.invoker"
  member   = "serviceAccount:${local.sa_email}"
}

# ── Cloud Run — ledger ────────────────────────────────────────────────────────

resource "google_cloud_run_v2_service" "ledger" {
  name     = "credit-proxy-ledger"
  location = var.region
  project  = var.project_id
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = local.sa_email

    scaling {
      min_instance_count = var.min_instances
      max_instance_count = var.max_instances
    }

    containers {
      image = var.ledger_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = var.ledger_memory
        }
        cpu_idle          = true
        startup_cpu_boost = true
      }

      env {
        name  = "LEDGER_ADDR"
        value = ":8080"
      }

      env {
        name = "POSTGRES_DSN"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.postgres_dsn.secret_id
            version = "latest"
          }
        }
      }

      startup_probe {
        http_get {
          path = "/health"
          port = 8080
        }
        initial_delay_seconds = 5
        timeout_seconds       = 3
        period_seconds        = 10
        failure_threshold     = 8
      }

      liveness_probe {
        http_get {
          path = "/health"
          port = 8080
        }
        initial_delay_seconds = 30
        timeout_seconds       = 3
        period_seconds        = 30
        failure_threshold     = 3
      }
    }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
  }

  depends_on = [
    google_project_service.run,
    google_secret_manager_secret_iam_member.run_sa_secret_access,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "ledger_invoker" {
  name     = google_cloud_run_v2_service.ledger.name
  location = var.region
  project  = var.project_id
  role     = "roles/run.invoker"
  member   = "serviceAccount:${local.sa_email}"
}

# ── Cloud Run — gateway ───────────────────────────────────────────────────────

resource "google_cloud_run_v2_service" "gateway" {
  name     = "credit-proxy-gateway"
  location = var.region
  project  = var.project_id
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = local.sa_email

    scaling {
      min_instance_count = var.gateway_min_instances
      max_instance_count = var.max_instances
    }

    containers {
      image = var.gateway_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = var.gateway_memory
        }
        cpu_idle          = true
        startup_cpu_boost = true
      }

      env {
        name  = "GATEWAY_ADDR"
        value = ":8080"
      }

      # Internal service URLs — stable after first deploy, injected directly from Terraform.
      env {
        name  = "USAGE_SERVICE_URL"
        value = google_cloud_run_v2_service.usage.uri
      }

      env {
        name  = "LLM_PROXY_URL"
        value = google_cloud_run_v2_service.llmproxy.uri
      }

      env {
        name  = "LEDGER_SERVICE_URL"
        value = google_cloud_run_v2_service.ledger.uri
      }

      startup_probe {
        http_get {
          path = "/health"
          port = 8080
        }
        initial_delay_seconds = 5
        timeout_seconds       = 3
        period_seconds        = 10
        failure_threshold     = 8
      }

      liveness_probe {
        http_get {
          path = "/health"
          port = 8080
        }
        initial_delay_seconds = 30
        timeout_seconds       = 3
        period_seconds        = 30
        failure_threshold     = 3
      }
    }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
  }

  depends_on = [
    google_project_service.run,
    google_cloud_run_v2_service.usage,
    google_cloud_run_v2_service.llmproxy,
    google_cloud_run_v2_service.ledger,
  ]
}

# Only novelsync-agents may invoke the gateway — no public internet access.
# Both services are in the same GCP project so INGRESS_INTERNAL_ONLY allows the call.
resource "google_cloud_run_v2_service_iam_member" "gateway_agents_invoker" {
  name     = google_cloud_run_v2_service.gateway.name
  location = var.region
  project  = var.project_id
  role     = "roles/run.invoker"
  member   = "serviceAccount:novelsync-agents-run@${var.project_id}.iam.gserviceaccount.com"
}
