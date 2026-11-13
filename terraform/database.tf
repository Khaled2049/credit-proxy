# The ledger's database login, its password and the connection string the
# service reads. The group it joins (creditproxy_ledger_app) is made by
# `ledger migrate`, which the deploy runs as the owner before this apply.
# Passwords and the owner URL are ephemeral or write-only, so neither is saved
# in Terraform state.
#
# The login is created with SQL through the postgresql provider, not Neon's API
# or console, which would add it to neon_superuser and its read and write on
# every table.

# Owner URL, synced by deploy.yml from the NEON_DATABASE_URL GitHub secret.
# Only CI reads it; no runtime has access.
data "google_secret_manager_secret" "postgres_owner_dsn" {
  secret_id = "creditproxy-postgres-owner-dsn"
  project   = var.project_id
}

ephemeral "google_secret_manager_secret_version" "owner_dsn" {
  secret  = data.google_secret_manager_secret.postgres_owner_dsn.secret_id
  project = var.project_id
}

locals {
  # [user, password, host, port, database]. The password must not need URL
  # escaping; Neon's generated ones do not.
  db_owner = regex(
    "^postgres(?:ql)?://([^:/@]+):([^@]+)@([^/:?]+)(?::([0-9]+))?/([^?]+)",
    ephemeral.google_secret_manager_secret_version.owner_dsn.secret_data,
  )
  ledger_role = "creditproxy_ledger"
}

provider "postgresql" {
  host            = local.db_owner[2]
  port            = coalesce(local.db_owner[3], "5432")
  database        = local.db_owner[4]
  username        = local.db_owner[0]
  password        = local.db_owner[1]
  sslmode         = "require"
  superuser       = false
  connect_timeout = 15
}

# A fresh value each run, used only when db_password_version changes; the role
# and the secret then take the same value from the same run.
ephemeral "random_password" "ledger" {
  length  = 40
  special = false
}

resource "postgresql_role" "ledger" {
  name                = local.ledger_role
  login               = true
  password_wo         = ephemeral.random_password.ledger.result
  password_wo_version = var.db_password_version
  # Membership belongs to postgresql_grant_role; without this the next apply revokes it.
  lifecycle {
    ignore_changes = [roles]
  }
}

resource "postgresql_grant_role" "ledger" {
  role       = postgresql_role.ledger.name
  grant_role = "creditproxy_ledger_app"
}

# The owner URL with the ledger login in place of the owner, so host, database,
# sslmode and channel binding stay identical.
resource "google_secret_manager_secret_version" "ledger_dsn" {
  secret = data.google_secret_manager_secret.postgres_dsn.id
  secret_data_wo = replace(
    ephemeral.google_secret_manager_secret_version.owner_dsn.secret_data,
    "/^(postgres(?:ql)?://)[^@]+@/",
    "$${1}${local.ledger_role}:${ephemeral.random_password.ledger.result}@",
  )
  secret_data_wo_version = var.db_password_version

  depends_on = [postgresql_role.ledger]
}
