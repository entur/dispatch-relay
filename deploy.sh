#!/usr/bin/env bash
# Usage: PROJECT=my-sandbox APP_ID=123456 KEY_FILE=app.pem ./deploy.sh
set -euo pipefail

: "${PROJECT:?}" "${APP_ID:?}" "${KEY_FILE:?}"
OWNER_ID=${OWNER_ID:-129931} # henrik242
REGION=${REGION:-europe-west1}
SERVICE=dispatch-relay
SECRET=dispatch-relay-app-key
SA=$SERVICE@$PROJECT.iam.gserviceaccount.com
g() { gcloud --project "$PROJECT" "$@"; }

g services enable run.googleapis.com secretmanager.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com

g iam service-accounts describe "$SA" >/dev/null 2>&1 ||
  g iam service-accounts create "$SERVICE" --display-name "dispatch relay"

if g secrets describe "$SECRET" >/dev/null 2>&1; then
  g secrets versions add "$SECRET" --data-file "$KEY_FILE"
else
  g secrets create "$SECRET" --data-file "$KEY_FILE"
fi
g secrets add-iam-policy-binding "$SECRET" \
  --member "serviceAccount:$SA" --role roles/secretmanager.secretAccessor >/dev/null

g run deploy "$SERVICE" --source "$(dirname "$0")" --region "$REGION" \
  --service-account "$SA" \
  --allow-unauthenticated \
  --max-instances 2 \
  --set-env-vars "GITHUB_APP_ID=$APP_ID,ALLOWED_OWNER_ID=$OWNER_ID" \
  --set-secrets "GITHUB_APP_PRIVATE_KEY=$SECRET:latest"

g run services describe "$SERVICE" --region "$REGION" --format 'value(status.url)'
