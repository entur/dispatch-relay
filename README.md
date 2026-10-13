# dispatch-relay

MVP of the OIDC dispatch relay from `dispatch-app-plan.md` (geocoder-pipeline). A public repo's workflow sends its
GitHub OIDC token; the relay verifies it, checks the target repo's `.github/dispatch-sources.yml`, and dispatches the
target workflow as a GitHub App with `inputs.sha` and `inputs.source_run_url` taken from the token.

Runs in a personal account and a sandbox GCP project, so no org app or platform resources are involved:

| Repo | Visibility | Role |
|---|---|---|
| `henrik242/dispatch-relay` | any | this service |
| `henrik242/dispatch-demo-app` | public | source, `build.yml` calls the relay |
| `henrik242/dispatch-demo-deploy` | private | target, `deploy.yml` plus the allowlist |

## Setup

1. Create the repos and push:
   ```sh
   gh repo create henrik242/dispatch-demo-app --public --source ../dispatch-demo-app --push
   gh repo create henrik242/dispatch-demo-deploy --private --source ../dispatch-demo-deploy --push
   ```
2. Set `repository_id` in `dispatch-demo-deploy/.github/dispatch-sources.yml` to
   `gh api repos/henrik242/dispatch-demo-app --jq .id` and push.
3. Create a GitHub App under Settings > Developer settings > GitHub Apps:
   - Webhook: inactive. No callback URL, no user authorization.
   - Repository permissions: Actions read and write; Single file read, path `.github/dispatch-sources.yml`;
     Metadata read.
   - Installable only on this account.
   - Note the App ID and generate a private key.
   - Install it on `dispatch-demo-deploy` only.
4. Deploy to a GCP project you own:
   ```sh
   PROJECT=<project> APP_ID=<app id> KEY_FILE=<key.pem> ./deploy.sh
   ```
   It prints the service URL. `--allow-unauthenticated` fails if the project sits under an org with domain
   restricted sharing; use a project outside it.
5. `gh variable set DISPATCH_RELAY_URL -R henrik242/dispatch-demo-app -b <url>`
6. Push to `dispatch-demo-app` main. The run summary links the deploy run.

Logs: `gcloud logging read 'resource.labels.service_name="dispatch-relay"' --project <project>`. One line per
request with decision and reason.

## Things to try

- Change a source field in the manifest (ref, workflow, id), push to the app: 403, reason in the log.
- Run the app workflow from a branch or a PR: no trigger job, or 403.
- Uninstall the app from the target: 403 `app not installed`.
- Dispatch by hand with an older SHA that is not on main: the deploy run fails its SHA check.

## Open checks

- Whether the Single file permission covers the Contents API read. If the manifest read gets 403, switch the app to
  Contents read and change `single_file` in `main.go`.

## Not in the MVP

Load balancer and hostname, Terraform, alerts and uptime check, per-source rate limiting, KMS signing.

## Development

`go test ./...` runs the relay against a fake issuer and a fake GitHub API.
