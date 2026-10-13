# dispatch-relay

Lets a public repo trigger a deploy workflow in a private repo without a secret. The source workflow sends its
GitHub OIDC token; the relay verifies it, checks the target's `.github/dispatch-sources.yml`, and dispatches the
target workflow as a GitHub App, with `sha` and `source_run_url` taken from the token. The design and threat model
are in [docs/design.md](docs/design.md).

| Repo | Role |
|---|---|
| `entur/dispatch-relay` | this service |
| `entur/dispatch-relay-deployment-config` | GCP project, deploy script, plan |
| `entur/dispatch-demo-app` | demo source: `build.yml` triggers, `probe.yml` exercises rejections |
| `henrik242/dispatch-demo-deploy` | demo target: `deploy.yml` and the allowlist |

## Onboard a repo

**Target** (`<app>-deployment-config`):
1. Add `.github/dispatch-sources.yml` to the default branch. See the demo target's file for the commented format;
   get the source's `repository_id` with `gh api repos/<owner>/<repo> --jq .id`.
2. Declare both `workflow_dispatch` inputs, `sha` and `source_run_url`. GitHub rejects the dispatch with 422 if
   either is missing.
3. Fail the run if `sha` is not on the source's `main`, as the demo's `deploy.yml` does.
4. Ask an org owner to add the repo to the app installation.

**Source:** add a job after the tests, with `id-token: write` on that job only:

```yaml
  trigger-deploy:
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    needs: [test]
    runs-on: ubuntu-24.04
    permissions:
      id-token: write
    steps:
      - uses: entur/dispatch-relay@<sha>
        with:
          target: entur/<app>-deployment-config
          # workflow: deploy.yml (default)
```

Pin the action by commit SHA. It fetches the job's OIDC token and posts it to the relay; see `action.yml`.

**A 403** carries no reason. Find it in the relay's log by the source run id:
`gcloud logging read 'jsonPayload.run_id="<run id>"' --project <relay project>`, or ask team-ror.

## GitHub App

Create it under Settings > Developer settings > GitHub Apps. Everything not listed stays at GitHub's defaults
(off, empty or no access):

| Field | Value |
|---|---|
| Webhook: Active | Off |
| Repository permissions | Actions: Read and write. Single file: Read, path `.github/dispatch-sources.yml`. Metadata: Read. |
| Where can it be installed? | Only on this account |

Then:
- Generate a private key, and note the Client ID. The relay accepts the Client ID or App ID as `GITHUB_APP_ID`.
  No client secret is needed.
- Install the app on the target repos only.
- The `.pem` goes to Secret Manager via `deploy.sh` in `dispatch-relay-deployment-config`. Delete the local
  copy afterwards.

## Demo

The demo repos and a personal app run against the relay in `ent-dispatch-dev`; the relay URL is the demo app's
`DISPATCH_RELAY_URL` variable.
- A push to `dispatch-demo-app` main deploys, and the run summary links the deploy run.
- `gh workflow run probe.yml -R entur/dispatch-demo-app` shows a rejection, with the reason in the log.

## Development

`go test ./...` runs the relay against a fake issuer and a fake GitHub API.
