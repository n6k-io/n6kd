#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

IMAGE=us-central1-docker.pkg.dev/n6k-io/containers/n6kd-demo:latest

docker build --pull --platform linux/amd64 -t "$IMAGE" .
docker push "$IMAGE"

gcloud run deploy n6kd-demo \
    --project n6k-io \
    --region us-central1 \
    --image "$IMAGE" \
    --set-env-vars "N6KD_API_KEY=${N6KD_API_KEY:-demo-token}"
