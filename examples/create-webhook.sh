#!/usr/bin/env sh
set -eu

: "${TARGET_URL:?set TARGET_URL to a public HTTP(S) webhook receiver}"

case "${TARGET_URL}" in
  http://* | https://*) ;;
  *)
    echo "TARGET_URL must use http or https" >&2
    exit 2
    ;;
esac

API_URL="${API_URL:-http://localhost:8080}"
IDEMPOTENCY_KEY="${IDEMPOTENCY_KEY:-demo-payment-123}"

curl --fail-with-body --include "${API_URL}/api/v1/webhooks" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: ${IDEMPOTENCY_KEY}" \
  --data "{\"target_url\":\"${TARGET_URL}\",\"event_type\":\"payment.completed\",\"payload\":{\"payment_id\":\"123\",\"amount\":5000}}"
