output "gateway_url" {
  description = "Public URL of the gateway Cloud Run service"
  value       = google_cloud_run_v2_service.gateway.uri
}

output "usage_url" {
  description = "Internal URL of the usage Cloud Run service"
  value       = google_cloud_run_v2_service.usage.uri
}

output "llmproxy_url" {
  description = "Internal URL of the llmproxy Cloud Run service"
  value       = google_cloud_run_v2_service.llmproxy.uri
}

output "ledger_url" {
  description = "Internal URL of the ledger Cloud Run service"
  value       = google_cloud_run_v2_service.ledger.uri
}

output "registry_url" {
  description = "Artifact Registry base URL for Docker images"
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${var.artifact_registry_repo}"
}

output "sa_email" {
  description = "Cloud Run service account email"
  value       = data.google_service_account.run_sa.email
}
