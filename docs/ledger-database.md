# Ledger database roles

The ledger service is creditProxy's only database user. In production it logs
in as `creditproxy_ledger`, which may append to and read `ledger_events` and
nothing else: no DELETE, no UPDATE except the upsert's no-op column, no DDL.

| Login | Used by | Can |
|---|---|---|
| `neondb_owner` | the deploy's migrate step and Terraform | Everything |
| `creditproxy_ledger` (group `creditproxy_ledger_app`) | ledger service | `SELECT`, `INSERT`, `UPDATE (idempotency_key)` on `ledger_events`; its sequence |

| Secret | Holds | Written by | Read by |
|---|---|---|---|
| GitHub `NEON_DATABASE_URL` | owner URL | you | the deploy |
| `creditproxy-postgres-owner-dsn` | owner URL | deploy (synced from GitHub) | Terraform |
| `creditproxy-postgres-dsn` | ledger URL | `terraform/database.tf` | ledger service |

**Never create the ledger login in the Neon console or with Neon's API.** Neon
adds such roles to `neon_superuser`, which can read and write every table and
overrides the grants above. Terraform creates it with SQL. As a backstop, the
ledger refuses to start with `MIGRATE_ON_START=false` if its login is a
superuser, can create roles or objects, bypasses row-level security, or is in
`neon_superuser`.

## How a deploy runs

1. Syncs `NEON_DATABASE_URL` into `creditproxy-postgres-owner-dsn`.
2. Runs the new ledger image as `ledger migrate` with the owner URL. That
   applies `schemaSQL` in `cmd/ledger/db.go`: the table, indexes, the
   `creditproxy_ledger_app` group and its grants. It is idempotent.
3. Terraform creates `creditproxy_ledger`, adds it to the group, writes its
   URL to `creditproxy-postgres-dsn`, then rolls the ledger revision.

## Rotating passwords

- **Ledger login:** bump `db_password_version` in `terraform/variables.tf` and
  deploy.
- **Owner:** reset `neondb_owner` in Neon, set the new URL as the GitHub
  `NEON_DATABASE_URL` secret, and deploy (or add it as a version of
  `creditproxy-postgres-owner-dsn`). Nothing at runtime uses the owner.

## Checking it

```sql
SELECT r.rolname, g.rolname FROM pg_auth_members m
  JOIN pg_roles r ON r.oid = m.member JOIN pg_roles g ON g.oid = m.roleid
 WHERE r.rolname = 'creditproxy_ledger';                 -- creditproxy_ledger_app only
SELECT has_table_privilege('creditproxy_ledger', 'ledger_events', 'DELETE');  -- f
SELECT usename, count(*) FROM pg_stat_activity
 WHERE usename IS NOT NULL GROUP BY 1;                   -- creditproxy_ledger, not neondb_owner
```

## Local development

Nothing changes: Compose connects as `postgres` and `MIGRATE_ON_START`
defaults to true. `LEDGER_TEST_DSN` runs the grant tests in `cmd/ledger`.
