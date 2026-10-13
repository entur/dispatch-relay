# dispatch-relay

MVP of the OIDC dispatch relay from `dispatch-app-plan.md` (geocoder-pipeline). A public repo's workflow sends its
GitHub OIDC token; the relay verifies it, checks the target repo's `.github/dispatch-sources.yml`, and dispatches the
target workflow as a GitHub App with `inputs.sha` and `inputs.source_run_url` taken from the token.

The GitHub App and the demo repos live in a personal account, so no org app is involved:

| Repo | Visibility | Role |
|---|---|---|
| `entur/dispatch-relay` | public | this service |
| `entur/dispatch-relay-deployment-config` | private | GCP manifests and deploy script |
| `henrik242/dispatch-demo-app` | public | source, `build.yml` calls the relay |
| `henrik242/dispatch-demo-deploy` | private | target, `deploy.yml` plus the allowlist |

## Setup

1. Create the demo repos and push:
   ```sh
   gh repo create henrik242/dispatch-demo-app --public --source ../dispatch-demo-app --push
   gh repo create henrik242/dispatch-demo-deploy --private --source ../dispatch-demo-deploy --push
   ```
2. Set `repository_id` in `dispatch-demo-deploy/.github/dispatch-sources.yml` to
   `gh api repos/henrik242/dispatch-demo-app --jq .id` and push.
3. Create a GitHub App under Settings > Developer settings > GitHub Apps > New GitHub App:

   | Field | Value |
   |---|---|
   | GitHub App name | `dispatch-relay`, or with a suffix if taken (names are global) |
   | Description | Optional, e.g. "Dispatch relay demo" |
   | Homepage URL | `https://github.com/entur/dispatch-relay` |
   | Redirect URI | Empty. The relay never acts as a user. |
   | Allow wildcard matching | Off |
   | Expire user authorization tokens | No effect without user authorization; leave as is |
   | Request user authorization (OAuth) during installation | Off |
   | Enable Device Flow | Off |
   | Setup URL | Empty |
   | Redirect on update | Off |
   | Webhook: Active | Off. URL and secret then stay empty. |
   | Repository permissions | Actions: Read and write. Single file: Read, path `.github/dispatch-sources.yml`. Metadata: Read (mandatory). Everything else: No access. |
   | Organization permissions | None |
   | Account permissions | None |
   | Enterprise permissions | None |
   | Subscribe to events | None (greyed out with the webhook off) |
   | Where can this GitHub App be installed? | Only on this account |

   After creating it:
   - Note the App ID; the relay accepts either the App ID or the Client ID as `GITHUB_APP_ID`.
   - Under "Private keys", click "Generate a private key". This downloads a `.pem` that the relay uses to sign
     its app JWTs; the deploy script uploads it to Secret Manager. Store it outside the repos so it can't be
     committed or uploaded with the source.
   - Skip "Generate a new client secret". Client secrets are for user sign-in (OAuth), which the relay doesn't do.
   - Install App > Only select repositories > `dispatch-demo-deploy`.
4. Deploy the relay; see `entur/dispatch-relay-deployment-config`. It prints the service URL.
5. `gh variable set DISPATCH_RELAY_URL -R henrik242/dispatch-demo-app -b <url>`
6. Push to `dispatch-demo-app` main. The run summary links the deploy run.

The relay logs one line per request with the decision and reason.

## Things to try

- Change a source field in the manifest (ref, workflow, id), push to the app: 403, reason in the log.
- Run the app workflow from a branch or a PR: no trigger job, or 403.
- Uninstall the app from the target: 403 `app not installed`.
- Dispatch by hand with an older SHA that is not on main: the deploy run fails its SHA check.

## Not in the MVP

Load balancer and hostname, Terraform, alerts and uptime check, per-source rate limiting, KMS signing.

## Development

`go test ./...` runs the relay against a fake issuer and a fake GitHub API.
