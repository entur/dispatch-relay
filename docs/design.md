# Design: OIDC dispatch relay

Lets a public repo trigger a deploy workflow in a private repo without storing a secret. Onboarding steps are in
the README; the plan for real use is `docs/production-plan.md` in `dispatch-relay-deployment-config`.

## 1. Problem

The TP decision of 2026-09-15 ("Deciding what's safe to expose in an open-source repository") splits apps into a
public source repo and a private `<app>-deployment-config` repo. The public repo has to tell the private one "main
is at SHA X, deploy it". Today that is done with PATs:
- tied to one person, so offboarding breaks deploys;
- they expire, and need org approval;
- a long-lived secret in a public repo;
- `Actions: write` allows far more than a dispatch.

Goals:
- The public repo stores no secret and has no GCP identity.
- The trigger is an explicit step after the tests pass.
- Only the target repo's owners can allow a source to trigger it.

## 2. Design

A stateless Cloud Run service acts as a GitHub App that is installed only on the private target repos:

```
public <app> build.yml                    dispatch-relay (Cloud Run)               private <app>-deployment-config
push to main: test -> trigger
  OIDC JWT (aud=entur-dispatch-relay)
  POST /v1/dispatch {target, workflow} -> 1. verify JWT
                                          2. installation token for the target
                                          3. read .github/dispatch-sources.yml <-  owned by the target team
                                          4. match claims against it
                                          5. workflow_dispatch               ---->  deploy.yml (sha, source_run_url)
  <- 200 {run_url}                        6. one log line
```

The caller sends only `target` and `workflow`. Everything else comes from verified claims or the target's
manifest:
- `sha` from the token;
- `source_run_url` built from `repository`, `run_id` and `run_attempt`;
- the dispatch ref from the manifest.

The target must declare both inputs, or GitHub rejects the dispatch with 422.

Alternatives considered:
- **PAT or app key as a (rotated) secret.** Any writer on the source can read it from a branch, and rotating
  doesn't help when they can read each new value.
- **Token rotator.** A cron job writes short-lived tokens into each source's environment secret. It is cheaper,
  but a valid token always sits in the public repo and the caller picks the SHA.
- **Webhook-driven app.** The trigger is implicit (guessed from `workflow_run`), and it needs a webhook secret.
- **Private repo polls.** Latency and noisy runs.
- **Relay behind IAM with workload identity.** That gives public repos a GCP identity, which the TP decision
  avoids.

## 3. Relay

**JWT checks** ([GitHub OIDC claims](https://docs.github.com/en/actions/reference/security/oidc)):
- **Signature:** RS256 only, against GitHub's JWKS (`<iss>/.well-known/jwks`). The key set is refetched hourly,
  and on an unknown `kid` at most once a minute. `jku`/`x5u` are ignored.
- **`iss`:** `https://token.actions.githubusercontent.com`.
- **`aud`:** `entur-dispatch-relay`, not the default `https://github.com/entur`, which every relying party shares.
- **Time:** `exp`, `nbf` and `iat`, with 60 s leeway.
- **Owner:** `repository_owner_id` is Entur's numeric org id. A repo transferred out of the org keeps its id, so
  this check matters.
- **Branch and runner:** `ref_type == branch`, `runner_environment == github-hosted`.
- **SHA:** must match `^[0-9a-f]{40}$`.
- **Manifest match:** `repository_id` (numeric, so it survives renames and blocks name reuse), `workflow_ref`,
  `ref` and `event_name`.

All of these run before any GitHub call.

**Tokens:** each request mints one installation token, scoped to the target repo with `single_file: read` and
`actions: write`. It is never returned or cached.

**No state.** There is no `jti` store. A token replayed within its lifetime (about 5 min plus leeway)
re-dispatches the same SHA. If a newer commit has landed in the meantime, that is a rollback, which the target's
approvals still gate. Add a store only if sikkerhet requires one.

**Responses:** callers get `200 {run_url}`, or a generic 403 (rejected) or 502 (GitHub or relay error). Every
request writes one log line with the decision, reason, target and workflow. Once the signature is valid, it also
has the source repo and id, ref, sha, event and run_id.

## 4. Allowlist in the target repo

`.github/dispatch-sources.yml` on the target's default branch, read on every request:

```yaml
version: 1
workflows:
  deploy.yml:                       # workflow in this repo
    ref: main                       # branch it runs on
    sources:
      - repository: entur/<app>     # informational
        repository_id: <numeric id> # authoritative
        workflow: .github/workflows/build.yml
        refs: [refs/heads/main]
        events: [push]              # default
```

- Only people who can already deploy can add a source. A public repo cannot grant itself a target.
- Removing an entry revokes access on the next request.
- The second gate is the app installation, on selected repos only, which an org owner manages.
- Rejected placements: a central allowlist repo (a bottleneck, edited by people who don't own the targets), and
  the public repo itself (it would grant itself access).

## 5. GitHub App

Org-owned, with no webhook and no user authorization. Repository permissions:
- **Actions: write**, for `workflow_dispatch`. There is no `Contents: write`, which is why the relay uses
  `workflow_dispatch` and not `repository_dispatch`.
- **Single file: read**, on `.github/dispatch-sources.yml`.
- **Metadata: read.**

It is installed on selected `*-deployment-config` repos.

## 6. Threat model

| Threat | Impact | Mitigation |
|---|---|---|
| Compromised workflow, or any writer, on the source `main` | Deploys a commit that is on `main`, as with the PAT. A force-push to an older commit, or re-running an old push run (up to 30 days), redeploys an older SHA. | A ruleset on the source `main`: PR, review, no force-push. The target's "SHA is on main" check and tst/prd approvals. |
| Other jobs in the source workflow | `workflow_ref` pins the file, not the job. Any job in it with `id-token: write`, including a called reusable workflow, can mint a valid token. | Grant `id-token: write` on the trigger job only. |
| Malicious fork PR | No token with `ref=main` and `event=push`. | `events: [push]` rejects `pull_request*`, `workflow_run` and `issue_comment`. The pinned `workflow_ref` covers `pull_request_target`. |
| Compromised shared action | It could steal a token, which works only against the relay, for about 5 min. | The step is inline, with no third-party code. |
| Replayed token | Re-dispatch within about 6 min, possibly a rollback (section 3). | Target approvals. |
| Leaked app key | On installed targets: dispatch any workflow, on any branch, with any inputs; cancel, delete or disable runs and workflows. No content write. A target that builds whatever `ref` it is given turns this into code execution with CI credentials. | Selected-repo install. Only the runtime service account can read the secret. Delete the local `.pem` after upload. Targets fail hard when the SHA isn't on main. Rotate on suspicion. |
| Relay compromise | Same as a leaked key. | Small Go service, stdlib plus a YAML parser, distroless image, a service account that can only read its one secret. |
| Abuse or DoS | Cost and GitHub rate limits. | Unauthenticated requests are rejected before any GitHub call. Cloud Run `max-instances`. |
| Information disclosure | None: errors are generic. | Reasons only in logs. |

## 7. Open questions for sikkerhet

1. Is a public endpoint with in-app OIDC verification acceptable, or is edge JWT validation required?
2. Is Secret Manager acceptable for the key, or is KMS signing required?
3. Is a ruleset on the source `main` (PR, review, no force-push) a precondition for onboarding?
4. Is a stateless relay acceptable (section 3), or is a `jti` store required?
5. Should `workflow_dispatch` on the source be allowed as an event? It lets any writer trigger a deploy.

## 8. MVP results (2026-10-13)

Setup: the relay in `ent-dispatch-dev`, a personal app, and the demo repos `henrik242/dispatch-demo-app` (public)
and `henrik242/dispatch-demo-deploy` (private).

A push to the demo app's `main` produced the deploy run within about 15 s, with the right `sha` and
`source_run_url`. Rejections were triggered from `probe.yml` in the demo app. Each got only a generic 403, and no
deploy run was created:

| Scenario | Logged reason |
|---|---|
| Workflow not in the manifest, on `main` | `workflow_ref ".../probe.yml@refs/heads/main" not allowed` |
| Target repo without the app | `app not installed on <repo>` |
| Target workflow not in the manifest | `workflow nope.yml not in manifest` |
| Listed workflow, `workflow_dispatch` instead of `push` | `event "workflow_dispatch" not allowed` |
| Listed workflow, branch `probe/x` | `ref "refs/heads/probe/x" not allowed` |
| Junk bearer token | `bad header: ...` |

Unit tests cover the rest of section 3: forged signature, `iss`, `aud`, time, owner, runner, tag, SHA format,
algorithm, unknown `kid` and extra body fields.

Observed:
- Manifest changes apply on the next request.
- A force-push of `main` to an older commit deployed that commit.
- `single_file` is enough to read the manifest through the Contents API.
- Org policy `gcp.resourceLocations` rejects `global` secrets, so secrets use user-managed replication in the
  region.
- An `allUsers` invoker binding is allowed in a self-service project.
- Cloud Run reserves `/healthz`, so the relay uses `/health/liveness`.

## 9. References

- [workflow_dispatch API](https://docs.github.com/en/rest/actions/workflows#create-a-workflow-dispatch-event);
  `return_run_details` came in the [2026-02-19 changelog](https://github.blog/changelog/2026-02-19-workflow-dispatch-api-now-returns-run-ids).
- [App permissions](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps) and
  [installation tokens](https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app).
- `entur/ai` `guides/playbooks/cloud-run-service.md`: the golden path (load balancer, central registry), which
  the MVP deliberately skips.
