# scaleset-runner

One Go Controller that registers a GitHub Actions runner Scale Set for each configured
repository on a personal account. It runs every Job in a fresh, sysbox-isolated Runner Container
that is removed afterwards. A fixed number of Slots is shared across all repositories. It is built
on [`github.com/actions/scaleset`](https://github.com/actions/scaleset) v0.5.0.

Specifications are in [`docs/`](docs/): the [requirements](docs/requirements.md),
[glossary](docs/glossary.md), [entity model](docs/entity_model.md) and
[use cases](docs/use_cases).

## Requirements on the Host

- Linux amd64 with Docker and [sysbox](https://github.com/nestybox/sysbox) 0.7 or later (`sysbox-runc` runtime).
- No inbound ports: the Controller only talks to GitHub over outbound HTTPS.

## Deploy

```sh
cd deploy
cp /path/to/your/.env .env      # credentials, see below
docker compose up -d --build --wait
```

On start the Controller:

1. builds the default Runner Image (`scaleset-runner/runner:<instance id>`) if it is missing;
2. removes Runner Containers and Runner Registrations left from a previous run;
3. deletes Scale Sets it recorded for repositories or names that are no longer configured;
4. creates, takes over, or updates one Scale Set per configured repository, then starts polling.

Workflows target a Scale Set by its name:

```yaml
jobs:
  test:
    runs-on: orpheus
```

### Configuration

All settings are environment variables.

| Variable | Default | Meaning |
|---|---|---|
| `SCALESET_INSTANCE_ID` | required | Distinguishes Controllers on one Host (`[a-z0-9-]`, ≤ 40). Labels its containers and Scale Sets. |
| `SCALESET_REPOSITORIES` | required | JSON array: `[{"repository":"owner/name","name":"optional scale set name","image":"optional runner image"}]` |
| `SCALESET_NAME` | — | Scale Set name for entries without `name`. |
| `SCALESET_GITHUB_APP_CLIENT_ID`, `SCALESET_GITHUB_APP_INSTALLATION_ID`, `SCALESET_GITHUB_APP_PRIVATE_KEY` | — | GitHub App installation covering all repositories (preferred). The key may use literal `\n`. |
| `SCALESET_GITHUB_TOKEN` | — | Personal access token, instead of the App. |
| `SCALESET_SLOTS` | `1` | Runner Containers running at once across all repositories (1–32). |
| `SCALESET_RUNNER_CPUS` / `SCALESET_RUNNER_MEMORY` | `2` / `4g` | Limits of every Runner Container. |
| `SCALESET_SHUTDOWN_TIMEOUT` | `5m` | How long running Jobs may finish on shutdown. Keep `stop_grace_period` above it. |
| `SCALESET_READINESS_TIMEOUT` | `120s` | Time a Runner Container may take to become ready. |
| `SCALESET_HEALTH_THRESHOLD` | `120s` | Maximum time without a successful poll before the Controller reports unhealthy. |
| `SCALESET_REFRESH_INTERVAL_DAYS` | `7` | Days between Runner Image refreshes. |
| `SCALESET_STATE_PATH` | `/var/lib/scaleset-runner/state.db` | State Store (SQLite) on its own volume. |
| `SCALESET_LISTEN_ADDR` | `:9090` | Health (`/healthz`) and metrics (`/metrics`) endpoint. Publish it on `127.0.0.1` only. |
| `SCALESET_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON. |
| `SCALESET_RUNTIME` | `sysbox-runc` | Container runtime of Runner Containers. |

An invalid Config names every problem in the log and exits with status 2.

### GitHub credentials

A repository-level Scale Set needs administration access to the repository. A classic PAT with
the `repo` scope is known to work. For a GitHub App, grant repository *Administration: read &
write* and install it on every configured repository (see open question Q-001 in the
requirements).

### Several Controllers on one Host

Give each Controller its own `SCALESET_INSTANCE_ID`, its own State Store volume and its own host
port. A second Controller with an Instance ID that is already in use exits at startup. Size the
Slot limits so that all Controllers together fit the Host.

## Securing public repositories (C-009)

Self-hosted runners execute whatever code a workflow checks out. Before you add a **public**
repository to the Config:

1. In *Settings → Actions → General → Fork pull request workflows from outside collaborators*,
   select **Require approval for all external contributors**.
2. Make sure no workflow runs fork pull request code with repository privileges. In particular,
   no `pull_request_target` (or `workflow_run`) workflow may check out the pull request's head
   and run it.
3. Keep `runs-on: <scale set name>` out of workflows that fork pull requests can trigger
   without approval.

If you cannot meet these settings, leave the repository out of the Config.

## Inside a Job

Every Runner Container has its own Docker daemon (sysbox), so `docker build` and
`docker compose` work without access to the Host's Docker. The default Runner Image adds the
compose plugin and an SSH client to `ghcr.io/actions/actions-runner`, so remote builds work too:

```yaml
- run: docker --host ssh://deploy@build-host build -t app .
```

Every Job starts with an empty Docker daemon and an empty home directory. Use the GitHub
Actions cache service to keep work between Jobs.

### Go modules and build cache

`actions/setup-go` caches `~/go/pkg/mod` and the build cache, keyed by `go.sum`:

```yaml
- uses: actions/checkout@v4
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
    cache: true
- run: go test ./...
```

Use `actions/cache` for other directories:

```yaml
- uses: actions/cache@v4
  with:
    path: |
      ~/go/pkg/mod
      ~/.cache/go-build
    key: go-${{ runner.os }}-${{ hashFiles('**/go.sum') }}
    restore-keys: go-${{ runner.os }}-
```

### Docker images and build layers

Build layers go to the cache service with buildx:

```yaml
- uses: docker/setup-buildx-action@v3
- uses: docker/build-push-action@v6
  with:
    context: .
    load: true
    tags: app:ci
    cache-from: type=gha
    cache-to: type=gha,mode=max
```

Pulled images of a compose stack (databases, Playwright) can be saved as a tarball:

```yaml
- id: images
  uses: actions/cache@v4
  with:
    path: /tmp/images.tar
    key: images-${{ hashFiles('compose.yaml') }}
- if: steps.images.outputs.cache-hit == 'true'
  run: docker load -i /tmp/images.tar
- if: steps.images.outputs.cache-hit != 'true'
  run: |
    docker compose pull
    docker save $(docker compose config --images) -o /tmp/images.tar
- run: docker compose up -d --wait
```

The cache holds 10 GB per repository. Whether restoring is faster than pulling depends on the
stack (open question Q-005).

### Per-repository Runner Images

`"image"` in a repository entry replaces the default Runner Image. It must be based on
`ghcr.io/actions/actions-runner` and must contain `dockerd` and the compose plugin if its Jobs
need them. It needs no entrypoint: the Controller copies its own entrypoint into every Runner
Container. Per-repository images are pulled when missing and again every refresh interval.

## Monitoring

- `GET /healthz` returns 200 when every repository has a polling Scale Set, every Scale Set
  polled within the health threshold, and no Runner Image is more than 49 days old. Otherwise
  it returns 503 and lists the problems.
- `GET /metrics` serves Prometheus metrics: `scaleset_runner_start_requests_waiting`,
  `scaleset_runner_slots_busy`, `scaleset_runner_assigned_jobs` and
  `scaleset_runner_job_duration_seconds` per Scale Set, plus `scaleset_runner_slots`.
  Start requests that wait for long periods mean the Slot limit is too low.
- Logs are JSON. Each lifecycle event names `repository` and `runner`, and from JobStarted on
  also `job_id`.
  Find a Job with `docker compose logs | grep '"job_id":"<id>"'`.

## Development

```sh
go test -race ./...
go build ./cmd/scaleset-runner
```

The scaling and Slot logic in `internal/controller` is tested against fake Docker and GitHub
clients.
