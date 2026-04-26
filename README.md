# keeppio-runner

A small replacement for Semaphore, scoped to keeppio's actual ops
needs: trigger Ansible playbooks via a web UI, stream their output
live, keep a history.

- One Go binary, one SQLite file. No external services.
- Reads available actions from `actions.yml` at the root of the
  cloned `keeppio-infrastructure` repo. Pulls + hard-resets to
  `origin/main` before each run.
- Deployed in pairs (one per env) on the same ops VPS. Each instance
  has its own bolt DB, repo clone, vault password file, and admin
  password.

## Configuration

| Env var                       | Required | Notes |
|-------------------------------|----------|-------|
| `RUNNER_ENV`                  | yes      | `staging` or `production`. Drives inventory path + UI label. |
| `RUNNER_REPO_PATH`            | yes      | Local path of the cloned infra repo. |
| `RUNNER_REPO_URL`             | yes      | HTTPS clone URL with embedded PAT. |
| `RUNNER_REPO_BRANCH`          | no       | Default `main`. |
| `RUNNER_VAULT_PASSWORD_FILE`  | yes      | Path to `.vault-pass-staging` / `.vault-pass-prod`. |
| `RUNNER_VAULT_LABEL`          | yes      | `staging` or `production`. |
| `RUNNER_DB_PATH`              | no       | Default `/data/runner.db`. |
| `RUNNER_LOG_DIR`              | no       | Default `/data/logs`. |
| `RUNNER_ADDR`                 | no       | Default `:3000`. |
| `RUNNER_ADMIN_PASSWORD`       | first boot only | Seeds the admin user. Ignored after. |
| `RUNNER_GITHUB_PAT`           | no       | Enables version-tag dropdowns by reading GHCR. |

## Local dev

```bash
go run . \
  RUNNER_ENV=staging \
  RUNNER_REPO_PATH=$HOME/Documents/Git/keeppio/keeppio-infrastructure \
  RUNNER_REPO_URL=https://x-access-token:$PAT@github.com/keeppio/keeppio-infrastructure.git \
  RUNNER_VAULT_PASSWORD_FILE=$HOME/Documents/Git/keeppio/keeppio-infrastructure/.vault-pass-staging \
  RUNNER_VAULT_LABEL=staging \
  RUNNER_DB_PATH=./data/runner.db \
  RUNNER_LOG_DIR=./data/logs \
  RUNNER_ADMIN_PASSWORD=changeme
```

Open http://localhost:3000 and sign in as `admin`.

## Production

Container image is `ghcr.io/keeppio/keeppio-runner:main`. Deployed by
the `runner` Ansible role in `keeppio-infrastructure`, which spawns one
container per env on the ops VPS, behind nginx with the same allowlist
pattern as Semaphore.
