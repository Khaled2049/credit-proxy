# GCP Deployment Guide

This document covers everything needed to deploy creditProxy to Google Cloud: what was built, why each decision was made, first-time setup commands, ongoing CI/CD, cost breakdown, and how to scale.

---

## Table of Contents

1. [What was built](#1-what-was-built)
2. [Architecture decisions](#2-architecture-decisions)
3. [Cost breakdown](#3-cost-breakdown)
4. [First-time setup](#4-first-time-setup)
5. [GitHub secrets to add](#5-github-secrets-to-add)
6. [How CI/CD works](#6-how-cicd-works)
7. [Verifying the deployment](#7-verifying-the-deployment)
8. [Scaling up](#8-scaling-up)
9. [Upgrading Redis to Cloud Memorystore](#9-upgrading-redis-to-cloud-memorystore)

---

## 1. What was built

### New files

```
creditProxy/
├── .github/workflows/
│   ├── deploy.yml         — CI/CD: build images → sync secrets → terraform → health check
│   └── pr-check.yml       — PR gate: go vet, go test, terraform fmt/validate
├── terraform/
│   ├── versions.tf        — Terraform version, GCS state backend, Google provider
│   ├── variables.tf       — All input variables with documented defaults
│   ├── main.tf            — Every GCP resource: APIs, SA, secrets, Neon DSN shell, Cloud Run ×4, IAM
│   └── outputs.tf         — Service URLs, registry URL, SA email
└── docs/
    └── deploy.md          — This file
```

### Modified files

**`pkg/httpx/httpx.go`** — Added `gcpIDToken()` helper that fetches an OIDC identity token from the GCE metadata server. `PostJSON()` now calls this and attaches `Authorization: Bearer <token>` to every outbound request. On GCP the token is fetched automatically. Locally the metadata server is unreachable so the function returns `""` and no header is set — docker-compose continues to work without changes.

**`cmd/usage/main.go`** — Changed `redis.NewClient(&redis.Options{Addr: redisAddr})` to `redis.ParseURL(redisURL)`. The original code only accepted a bare `host:port` address. Upstash Redis uses a TLS URL (`rediss://:<password>@<host>:<port>`), which requires `ParseURL`. Locally the default is `redis://redis:6379` which is functionally identical to the old value.

**`docker-compose.yml`** — Changed `REDIS_ADDR: "redis:6379"` to `REDIS_URL: "redis://redis:6379"` to match the new env var name. No functional change for local development.

---

## 2. Architecture decisions

### Four separate Cloud Run services

Each of the four services (gateway, usage, llmproxy, ledger) is deployed as its own Cloud Run service rather than one combined container. This means:

- Each service scales independently. During a traffic spike on LLM calls, only llmproxy scales out — usage and ledger are not affected.
- Each service can be updated independently without redeploying the others.
- Memory and CPU limits are tuned per service (llmproxy gets 512 MB because it processes larger payloads; the others get 256 MB).
- If one service has a bug and crashes, the others keep running.

### Gateway: min_instances = 1; internal services: min_instances = 0

The three internal services (usage, llmproxy, ledger) are configured with `min_instance_count = 0` — they scale to zero when idle and pay nothing.

The gateway uses `min_instance_count = 1` (controlled by the `gateway_min_instances` variable, default 1). This is required for Cloud Run v2: when a Cloud Run v2 service scales to zero and back, GFE (Google's frontend) can lose its backend route mapping, causing all subsequent requests to return a Google 404 HTML page rather than being dispatched to the service. Keeping one gateway instance warm prevents this. At 256 MB with `cpu_idle = true`, the idle cost is approximately $2–4/month.

### cpu_idle = true

With `cpu_idle = true`, Cloud Run does not charge for CPU while the container is waiting for a request (idle between requests). CPU is only charged during active request processing. This halves effective CPU costs at low-to-medium traffic.

### llmproxy concurrency capped at 10

Cloud Run's default is 80 concurrent requests per instance. For gateway/usage/ledger (fast operations: milliseconds) this is fine. For llmproxy, each request is an outbound LLM call that can take 5–30 seconds. If 80 requests queued behind a slow LLM call, they would all time out. Capping at 10 means Cloud Run scales out to a new instance sooner, preventing head-of-line blocking.

### Internal services locked to INGRESS_TRAFFIC_INTERNAL_ONLY

usage, llmproxy, and ledger cannot be reached from the public internet at the network layer. Only gateway is public. This means:

- Even if someone discovers the internal Cloud Run URLs, they cannot call them directly.
- The attack surface is limited to one endpoint (gateway).

However, `INGRESS_TRAFFIC_INTERNAL_ONLY` is a network restriction, not an IAM authentication check. Cloud Run IAM is still enforced — callers still need a valid identity token. That is why `pkg/httpx/httpx.go` was modified to attach an OIDC token. The gateway's service account (`credit-proxy-run`) is granted `roles/run.invoker` on each internal service, so its token is accepted.

### One shared service account for all services

All four Cloud Run services run as `credit-proxy-run@story-6f89f.iam.gserviceaccount.com`. This simplifies IAM — one SA to manage, one set of permissions to audit. The alternative (one SA per service with minimal permissions each) is stricter but adds significant maintenance overhead at this stage. You can introduce per-service SAs later if a security audit requires it.

### Upstash Redis instead of Cloud Memorystore

Cloud Memorystore (GCP's managed Redis) lives inside a VPC. Cloud Run is serverless and does not live in a VPC by default. To connect them you need a Serverless VPC Access Connector, which runs two always-on `e2-micro` VMs. This adds ~$45/month in fixed infrastructure cost regardless of traffic.

Upstash is a managed Redis-as-a-service that exposes Redis over HTTPS/TLS — no VPC required. The free tier covers 10,000 commands per day and 256 MB of data, which is plenty for development and early production. When you outgrow Upstash, see [section 9](#9-upgrading-redis-to-cloud-memorystore) for the migration path to Cloud Memorystore.

The one trade-off of Upstash over Memorystore: Upstash adds ~1–5 ms of network latency per Redis command compared to ~0.1 ms for a VPC-local Memorystore instance. For credit reservation (the most latency-sensitive operation), this adds a few milliseconds per LLM request — acceptable at startup.

### Neon serverless Postgres instead of Cloud SQL

Cloud SQL `db-f1-micro` costs ~$9.47/month and cannot be paused — it's always on regardless of traffic. The ledger service only needs Postgres for an append-only audit table, which is an excellent fit for a serverless database that scales to zero.

Neon is a managed serverless Postgres provider that exposes a standard TCP connection string (`postgresql://...?sslmode=require`) — no VPC, no Cloud SQL connector, no Unix socket path. The ledger service connects over the public internet with TLS, the same way it connects to the local Postgres container in `docker-compose.yml`. No code changes required.

The `POSTGRES_DSN` is stored in Secret Manager as `creditproxy-postgres-dsn`. The workflow creates and syncs it from the `NEON_DATABASE_URL` GitHub secret — the same pattern as `GEMINI_API_KEY` and the other API keys. Terraform reads it as a data source to apply IAM bindings, but never sees the raw DSN value. `NEON_DATABASE_URL` must be set in GitHub secrets before the first push to `main`.

Neon free tier: 0.5 GB storage, compute auto-pauses after 5 minutes idle. The ledger only needs compute during AI requests — perfect fit.

### Terraform state in GCS

Terraform state is stored in `gs://story-6f89f-tfstate` with prefix `credit-proxy`. This is the same bucket used by the other NovelSync repos (novelsync-agents uses prefix `novelsync-agents`, frontend uses prefix `frontend/state`). State is isolated by prefix — they do not interfere with each other. GCS provides locking, versioning, and is free for the storage amounts involved.

### Image tagging with git SHA, never `:latest`

Every deploy tags images with `$GITHUB_SHA` (e.g. `gateway:a3f92bc1...`). Cloud Run creates a new revision only when the image reference changes. If you push `:latest` twice, Cloud Run sees the same image tag and may not deploy new code. SHA tagging guarantees every push to `main` creates a new revision and triggers a rollout.

### Secrets: shells pre-created, versions managed separately

All secrets (`creditproxy-gemini-api-key`, `creditproxy-postgres-dsn`, etc.) follow the same pattern: the workflow creates the shell if missing and updates the version only when the value has changed. Terraform creates the secret shell for `creditproxy-postgres-dsn` so IAM can be applied before the first deploy, but the workflow owns the version. You can rotate any secret by updating the GitHub secret and rerunning the workflow.

### WIF service account roles scoped to least privilege

The GitHub Actions WIF service account has:
- `roles/iam.serviceAccountUser` — scoped to `credit-proxy-run` SA specifically, not the whole project. This means GitHub Actions can only impersonate this one SA when deploying Cloud Run services, not any other SA in the project.
- `roles/secretmanager.secretVersionAdder` + `roles/secretmanager.secretAccessor` — instead of `roles/secretmanager.admin`. GitHub Actions can add new secret versions and read them (to check if values changed), but cannot delete secrets or modify their metadata.
- `roles/run.admin`, `roles/artifactregistry.admin` — needed to manage the respective resources via Terraform.
- `roles/storage.objectAdmin` — to read/write Terraform state in GCS.

---

## 3. Cost breakdown

### At startup / low traffic

| Component | Monthly cost | Notes |
|---|---|---|
| Cloud Run — gateway (1 warm instance) | ~$2–4 | One always-on 256 MB instance with `cpu_idle=true`. Memory billed continuously; CPU billed only during requests. Required to prevent Cloud Run v2 GFE routing loss on scale-to-zero. |
| Cloud Run — usage, llmproxy, ledger | $0 | Scale to zero. Free tier: 2M requests/month, 360K GB-seconds, 180K vCPU-seconds. |
| Neon (free tier) | $0 | Serverless Postgres, scales to zero. 0.5 GB storage, compute auto-pauses after 5 min idle. |
| Upstash Redis free tier | $0 | 10,000 commands/day, 256 MB. Covers ~5,000 AI requests/day (2 Redis ops per request). |
| Artifact Registry | $0 | Free 0.5 GB/month. 4 small Go images ≈ 20–30 MB total. |
| Secret Manager | $0 | Free for first 6 active secret versions/month. |
| GCS (Terraform state) | ~$0.02 | Shared bucket, negligible. |
| **Total** | **~$2–4/month** | |

### When traffic grows (above Cloud Run free tier)

Cloud Run pricing above the free tier:
- Requests: $0.40 per 1 million
- CPU: $0.00002400 per vCPU-second
- Memory: $0.00000250 per GB-second

At 10 million requests/month with average 500 ms processing time per request on 1 vCPU / 256 MB:
- Request cost: ~$3.20
- CPU cost: ~$120 (10M × 0.5s × $0.000024)
- Memory cost: ~$3.20

Cloud Run auto-scales to handle this load automatically. You do not need to change any configuration — just let it scale.

At this traffic level (10M requests/month), the Upstash free tier would be exhausted. Upstash pay-as-you-go pricing is $0.20 per 100,000 commands. At 2 commands per AI request: 20M commands × $0.002 = ~$40/month. This is when migrating to Cloud Memorystore makes sense — see [section 9](#9-upgrading-redis-to-cloud-memorystore).

### Neon scaling costs

Neon scales automatically. On the free tier (0.5 GB, shared compute) this stack will run indefinitely. When you outgrow the free tier, Neon's paid tier starts at $19/month for 10 GB storage and dedicated compute. At that point you likely have enough traffic to justify it.

---

## 4. First-time setup

These steps run **once** from your terminal before the first push to `main`. You must be authenticated as a GCP project owner.

### Step 1 — Verify gcloud authentication

```bash
gcloud auth list
gcloud config set project story-6f89f
```

### Step 2 — Enable bootstrap APIs

These two APIs must be enabled manually because Terraform itself needs them to authenticate and manage other resources.

```bash
gcloud services enable iam.googleapis.com \
  cloudresourcemanager.googleapis.com \
  --project=story-6f89f
```

### Step 3 — Create the Cloud Run service account

This SA is the identity that all four Cloud Run services run as. It will be granted access to secrets and the ability to invoke the internal services.

```bash
gcloud iam service-accounts create credit-proxy-run \
  --display-name="Credit Proxy Cloud Run SA" \
  --project=story-6f89f
```

### Step 4 — Grant the WIF service account least-privilege roles

The WIF service account is what GitHub Actions authenticates as. You need its email address — check your GitHub repository secrets for `WIF_SERVICE_ACCOUNT`.

```bash
WIF_SA="<paste the email from your WIF_SERVICE_ACCOUNT GitHub secret here>"
```

Grant `roles/iam.serviceAccountUser` **scoped to the `credit-proxy-run` SA only** (not the whole project). This allows GitHub Actions to deploy Cloud Run services that run as `credit-proxy-run`, but prevents it from impersonating any other SA in the project.

```bash
gcloud iam service-accounts add-iam-policy-binding \
  credit-proxy-run@story-6f89f.iam.gserviceaccount.com \
  --member="serviceAccount:${WIF_SA}" \
  --role="roles/iam.serviceAccountUser" \
  --project=story-6f89f
```

Grant project-level roles. Note `secretmanager.secretVersionAdder` and `secretmanager.secretAccessor` are used instead of `secretmanager.admin` — GitHub Actions can add/read secret versions but cannot delete secrets or change their replication policy.

```bash
for ROLE in \
  roles/run.admin \
  roles/artifactregistry.admin \
  roles/secretmanager.secretVersionAdder \
  roles/secretmanager.secretAccessor \
  roles/storage.objectAdmin; do
  gcloud projects add-iam-policy-binding story-6f89f \
    --member="serviceAccount:${WIF_SA}" \
    --role="$ROLE"
  echo "Granted $ROLE"
done
```

### Step 5 — Create the Artifact Registry repository

The deploy workflow checks for this and creates it if missing, but creating it now avoids a failure if the WIF SA doesn't yet have permission during the first workflow run.

```bash
gcloud artifacts repositories create credit-proxy \
  --repository-format=docker \
  --location=us-central1 \
  --project=story-6f89f \
  --description="Docker images for creditProxy services"
```

### Step 6 — Create the Secret Manager secret shells

These must be created as a project owner. The WIF service account (used by GitHub Actions) can add versions to existing secrets but cannot create new secret shells.

```bash
for SECRET in \
  creditproxy-gemini-api-key \
  creditproxy-openai-api-key \
  creditproxy-anthropic-api-key \
  creditproxy-upstash-redis-url \
  creditproxy-postgres-dsn; do
  gcloud secrets create "${SECRET}" \
    --replication-policy=automatic \
    --project=story-6f89f
  echo "Created ${SECRET}"
done
```

This creates empty shells with no versions. The actual values (API keys, Redis URL, Postgres DSN) are added by the deploy workflow from the corresponding GitHub secrets on first push to `main`. The WIF service account can add versions (`roles/secretmanager.secretVersionAdder`) but cannot create secret shells, which is why this step runs as a project owner.

### Step 7 — Confirm the Terraform state bucket exists

This bucket is shared with novelsync-agents. If it already exists (it should), this command succeeds silently.

```bash
gcloud storage buckets describe gs://story-6f89f-tfstate
```

If it does not exist (first NovelSync repo being deployed to this project):

```bash
gcloud storage buckets create gs://story-6f89f-tfstate \
  --location=us-central1 \
  --uniform-bucket-level-access

gcloud storage buckets update gs://story-6f89f-tfstate --versioning
```

### Step 8 — Create an Upstash Redis database

1. Go to [upstash.com](https://upstash.com) and create a free account.
2. Create a new Redis database:
   - Name: `creditproxy`
   - Type: Regional
   - Region: `us-central1` (closest to Cloud Run)
   - Enable TLS: yes (required)
3. From the database details page, copy the **Redis URL** — it looks like:
   ```
   rediss://default:<password>@<hostname>.upstash.io:<port>
   ```
4. Add it as the `UPSTASH_REDIS_URL` GitHub secret (see next section).

### Step 9 — Create a Neon database

1. Go to [neon.tech](https://neon.tech) and create a free account.
2. Create a new project:
   - Name: `creditproxy`
   - Postgres version: 16
   - Region: `US East (N. Virginia)` — closest available to `us-central1`
3. From the **Connection Details** panel, copy the connection string. It looks like:
   ```
   postgresql://user:password@ep-xxxx.us-east-2.aws.neon.tech/neondb?sslmode=require
   ```
4. Add it as the `NEON_DATABASE_URL` GitHub secret (see next section).

The ledger service auto-creates its schema on startup — no manual migration needed.

---

## 5. GitHub secrets to add

Go to your GitHub repository → **Settings → Secrets and variables → Actions → New repository secret** for each of the following.

| Secret name | How to get it |
|---|---|
| `WIF_PROVIDER` | Already exists — shared from other NovelSync repos |
| `WIF_SERVICE_ACCOUNT` | Already exists — shared from other NovelSync repos |
| `GEMINI_API_KEY` | [Google AI Studio](https://aistudio.google.com/app/apikey) |
| `OPENAI_API_KEY` | [OpenAI Platform](https://platform.openai.com/api-keys) |
| `ANTHROPIC_API_KEY` | [Anthropic Console](https://console.anthropic.com/settings/keys) |
| `NEON_DATABASE_URL` | Connection string from Neon dashboard (step 9 above) |
| `UPSTASH_REDIS_URL` | Redis URL from Upstash dashboard (step 8 above) |

`NEON_DATABASE_URL` must be set before the first push to `main` — the ledger service cannot start without a Postgres DSN.

`OPENAI_API_KEY` and `ANTHROPIC_API_KEY` can be left blank if you only plan to use Gemini. The workflow skips syncing a secret when its value is empty. The llmproxy service will still receive the env var but it will be empty, which is fine — those providers simply won't work until a real key is added.

---

## 6. How CI/CD works

### On every push to `main`

The `deploy.yml` workflow runs automatically. It:

1. Authenticates to GCP using Workload Identity Federation — no long-lived service account keys stored in GitHub. GitHub's OIDC token is exchanged for a short-lived GCP token.

2. Creates the Artifact Registry repository if it doesn't exist yet.

3. Builds and pushes 4 Docker images in a loop, one per service. The single `Dockerfile` in the repo root builds any service via the `SERVICE` build argument. Each image is tagged with the git commit SHA.

4. Creates the Terraform state bucket if it doesn't exist.

5. Creates or updates secrets in Secret Manager — API keys and the Neon DSN (`creditproxy-postgres-dsn`). Values are only updated if they have changed since the last deploy (avoids creating unnecessary secret versions and the associated audit log noise).

6. Runs `terraform init` to configure the GCS backend.

7. Runs `terraform plan` with the 4 image URIs (SHA-tagged) as variables.

8. Runs `terraform apply` to converge GCP state. On first run this creates IAM bindings and all 4 Cloud Run services. On subsequent runs it updates only what has changed (typically just the image tags).

9. Health-checks the gateway's `/healthz` endpoint with 5 retries and 15-second waits. The internal services (usage, llmproxy, ledger) are `INGRESS_TRAFFIC_INTERNAL_ONLY` — they are not reachable from the GitHub runner, so only gateway is checked externally. Cloud Run's startup probe validates the internal services before they receive traffic.

### On every pull request to `main`

The `pr-check.yml` workflow runs:
- `go vet ./...` — catches common Go mistakes
- `go test ./...` — runs all unit tests
- `terraform fmt -check` — ensures Terraform files are formatted
- `terraform validate` — validates Terraform syntax without connecting to GCP (uses `-backend=false`)

PRs that fail these checks are blocked from merging.

### Skipping deploys for documentation-only changes

The deploy workflow ignores pushes where only `.md` files, `LICENSE`, or `.gitignore` change. This prevents a full 5-minute deploy cycle for a README edit.

---

## 7. Verifying the deployment

### Gateway (public — call directly)

```bash
# Get the gateway URL from Terraform output
GW=$(cd terraform && terraform output -raw gateway_url)

# Health check
curl "${GW}/healthz"
# Expected: {"status":"ok","version":"...","commit":"..."}

# End-to-end test with mock LLM (no API key needed)
curl -X POST "${GW}/v1/generate" \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": "smoke-test",
    "prompt": "Hello world",
    "max_output_tokens": 20,
    "force_mock": true
  }'
# Expected:
# {
#   "reservation_id": "res_...",
#   "idempotency_key": "idem_...",
#   "estimated_credits": ...,
#   "actual_credits": ...,
#   "byok": false,
#   "response": {"output":"...","model":"mock","usage":{"prompt_tokens":...,"completion_tokens":...,"total_tokens":...}}
# }
```

### Internal services (use Cloud Run proxy)

Internal services are not reachable directly from your laptop. Use `gcloud run services proxy` which opens an authenticated local tunnel.

```bash
# Terminal 1 — proxy to usage service
gcloud run services proxy credit-proxy-usage \
  --region=us-central1 --project=story-6f89f --port=8091

# Terminal 2 — proxy to ledger service
gcloud run services proxy credit-proxy-ledger \
  --region=us-central1 --project=story-6f89f --port=8093
```

Then in a third terminal:

```bash
# Check credits were created for smoke-test user (new users get 10,000 free credits)
curl http://localhost:8091/v1/users/smoke-test/balance
# Expected: {"user_id":"smoke-test","available_credits":9950,...}

# Check audit log
curl http://localhost:8093/v1/users/smoke-test/ledger
# Expected: array with credits_reserved + credits_committed events
```

### Check Neon data directly

```bash
# Use the connection string from the NEON_DATABASE_URL GitHub secret
psql "$NEON_DATABASE_URL" \
  -c "SELECT event_type, credits, created_at FROM ledger_events ORDER BY created_at DESC LIMIT 10;"
```

Or open the Neon dashboard → your project → **Tables** to browse `ledger_events` visually.

### Run the BDD smoke suite

```bash
GATEWAY_URL=$GW go test -v -tags smoke ./tests/smoke/
```

### Deployment checklist

- [ ] `GET /healthz` on gateway returns `{"status":"ok"}`
- [ ] `POST /v1/generate` with `force_mock: true` returns 200 and `actual_credits > 0`
- [ ] Usage balance for smoke-test user decremented correctly
- [ ] Ledger shows `credits_reserved` and `credits_committed` events
- [ ] Neon `ledger_events` table has rows
- [ ] All 5 Secret Manager secrets have a latest version
- [ ] Artifact Registry shows 4 images tagged with the deployed SHA

---

## 8. Scaling up

### Increase max instances

By default `max_instances = 5`. To allow more concurrent requests, increase this in `terraform/variables.tf` or pass `-var="max_instances=20"` in the deploy workflow.

### Prevent cold starts on gateway

Gateway already runs with `min_instances = 1` (the `gateway_min_instances` variable defaults to 1). This is required — Cloud Run v2 loses GFE backend routing when a service scales to zero and back, causing 404s from Google's frontend rather than your service. Do not set this to 0.

To revert to zero for cost savings in a dev environment:

```bash
terraform apply -var="gateway_min_instances=0" ...
```

### Increase memory / CPU for llmproxy

LLM responses can be large. If llmproxy is running out of memory:

```hcl
# terraform/variables.tf
variable "llmproxy_memory" {
  default = "1Gi"  # was 512Mi
}
```

Valid Cloud Run memory values: `256Mi`, `512Mi`, `1Gi`, `2Gi`, `4Gi`, `8Gi`.

### Scale Neon

Neon scales automatically. If you hit free tier limits (0.5 GB storage or compute throttling under high concurrent writes), upgrade to a paid Neon plan in the Neon dashboard — no Terraform changes needed.

### Multi-region deployment

Cloud Run supports multi-region by deploying the same service to multiple regions and using a Global Load Balancer to route traffic. This is a significant infrastructure addition — not needed until you have users in multiple geographies.

---

## 9. Upgrading Redis to Cloud Memorystore

When Upstash becomes a bottleneck (high command volume or latency requirements below ~1 ms), migrate to Cloud Memorystore. This costs ~$45/month more but eliminates the external dependency and cuts Redis latency to ~0.1 ms.

### Steps

**1. Revert the one-line code change in `cmd/usage/main.go`:**

```go
// Change back to:
redisAddr := getenv("REDIS_ADDR", "redis:6379")
rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
```

And in `docker-compose.yml`:
```yaml
REDIS_ADDR: "redis:6379"
```

**2. Add VPC resources.** Create `terraform/vpc.tf`:

```hcl
resource "google_compute_network" "vpc" {
  name                    = "credit-proxy-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "subnet" {
  name          = "credit-proxy-subnet"
  ip_cidr_range = "10.8.0.0/28"
  region        = var.region
  network       = google_compute_network.vpc.self_link
}

resource "google_vpc_access_connector" "connector" {
  name          = "credit-proxy-conn"
  region        = var.region
  subnet { name = google_compute_subnetwork.subnet.name }
  min_instances = 2
  max_instances = 3
}

resource "google_redis_instance" "cache" {
  name           = "credit-proxy-redis"
  tier           = "BASIC"
  memory_size_gb = 1
  region         = var.region
  authorized_network = google_compute_network.vpc.self_link
  redis_version  = "REDIS_7_0"
}
```

**3. Update all 4 Cloud Run services in `main.tf`** to attach the VPC connector and set `REDIS_ADDR`:

In usage service, replace the `REDIS_URL` secret ref with:
```hcl
env {
  name  = "REDIS_ADDR"
  value = "${google_redis_instance.cache.host}:6379"
}
```

Add to every Cloud Run service template:
```hcl
vpc_access {
  connector = google_vpc_access_connector.connector.id
  egress    = "PRIVATE_RANGES_ONLY"
}
```

**4. Grant WIF SA the extra roles:**

```bash
for ROLE in roles/vpcaccess.admin roles/redis.admin; do
  gcloud projects add-iam-policy-binding story-6f89f \
    --member="serviceAccount:${WIF_SA}" --role="$ROLE"
done
```

**5. Enable extra APIs** (add to `main.tf`):
```hcl
resource "google_project_service" "redis"    { service = "redis.googleapis.com" }
resource "google_project_service" "vpcaccess" { service = "vpcaccess.googleapis.com" }
resource "google_project_service" "servicenetworking" { service = "servicenetworking.googleapis.com" }
```

**6. Push to main.** The deploy workflow handles the rest.

**Note:** Memorystore creation takes ~5 minutes. The first Terraform apply after adding these resources will be slow. Subsequent deploys are fast.
