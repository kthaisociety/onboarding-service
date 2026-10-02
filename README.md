# onboarding-service

Onboards and offboards KTHAIS members: when landingpage-backend accepts someone, it notifies this service,
which emails them a link to confirm their kth.se address, then creates their Google Workspace account
(kthais.com) and group memberships, and sends their account details and the next steps. It also
deactivates and deletes accounts (Google Workspace and Mattermost) on offboarding.

Internal only: landingpage-backend and the frontend call it on Dokploy's network; members reach it through
the frontend's onboarding pages. Every call except `/health` and the portal is authenticated with the
shared `ONBOARDING_SERVICE_SECRET` (`X-Service-Secret`).

## API

| | |
|---|---|
| `GET /health` | liveness |
| `POST /notify` | landingpage-backend: someone was accepted |
| `POST /portal/submit-email`, `GET`/`POST /portal/confirm` | the member's onboarding pages (through the frontend) |
| `/internal/onboarding/…` | admin actions: records, cancel, restart, retry provisioning, delete a record, email settings (read, edit, preview) |
| `/internal/offboarding/deactivate`, `/delete` | offboarding |

## Configuration

All from environment variables (see `.env.example` and `internal/config/config.go`):

| Variable | |
|---|---|
| `HOST`, `PORT` | listen address; `PORT` defaults to 8000. Use `HOST=127.0.0.1` locally |
| `DB_PATH` | SQLite file; `/data/onboarding.db` in the image |
| `ONBOARDING_SERVICE_SECRET` | shared with landingpage-backend, both directions |
| `BACKEND_URL` | landingpage-backend's API base, no trailing slash |
| `PORTAL_BASE_URL` | the frontend's onboarding pages, no trailing slash |
| `GOOGLE_ADMIN_SERVICE_ACCOUNT_JSON` (base64 or raw) or `…_JSON_PATH` | the domain-wide-delegation service account |
| `GOOGLE_ADMIN_IMPERSONATE_AS` | the Workspace user it acts as (a dedicated, narrowly privileged admin) |
| `MATTERMOST_URL`, `MATTERMOST_BOT_TOKEN` | Mattermost, for offboarding |

## Development

```sh
cp .env.example .env    # fill in; HOST=127.0.0.1
go run ./cmd/api
go vet ./... && go test -race ./...
```

## Delivery

Built, released and deployed through the KTHAIS pipeline
([kthaisociety/infrastructure: docs/app-delivery.md](https://github.com/kthaisociety/infrastructure/blob/main/docs/app-delivery.md)):

- **PRs:** Conventional Commit titles (`feat: …`, `fix: …`), `go vet` and `go test -race`, squash merges.
- **Every merge to `main`:** tests, then the image `ghcr.io/kthaisociety/onboarding-service:sha-<7>`,
  deployed to **staging** through
  [kthaisociety/deployments](https://github.com/kthaisociety/deployments) (`build.yml`).
- **Releases:** release-please keeps a release PR with the next version and `CHANGELOG.md`. A code owner
  (in `.github/CODEOWNERS`) merging it publishes `vX.Y.Z` and tags that commit's image `X.Y.Z`.
  Production deploys from releases are switched on with the move off the old Dokploy app
  ([migration](https://github.com/kthaisociety/infrastructure/blob/main/docs/onboarding-service-migration.md)).
- **Config and secrets** live in `kthaisociety/deployments` (`projects/onboarding-service/`) and OpenBao,
  not here.
