#!/bin/sh
# fake app log stream; prod logs the webhook failure
i=0
while true; do
  i=$((i+1))
  echo "$(date -u +%FT%TZ) INFO  api GET /orders 200 ${i}ms"
  if [ "$APP_ENV" = prod ] && [ $((i%3)) -eq 0 ]; then
    echo "$(date -u +%FT%TZ) ERROR webhook payfast payment.succeeded rejected: signature mismatch (expected secret v4, got v3) order=$((40000+i)) -> left pending_payment"
  fi
  if [ "$APP_ENV" = staging ] && [ $((i%4)) -eq 0 ]; then
    echo "$(date -u +%FT%TZ) ERROR GET /admin/refunds 500: pq: column orders.refund_reason does not exist"
  fi
  if [ "$APP_ENV" = prod ] && [ $((i%5)) -eq 0 ]; then
    echo "$(date -u +%FT%TZ) WARN  mailer send order_confirmation region=eu -> 400 Bad Request (will retry)"
  fi
  if [ $((i%7)) -eq 0 ] && { [ "$APP_ENV" = prod ] || [ "$APP_ENV" = staging ]; }; then
    echo "$(date -u +%FT%TZ) WARN  mailer send order_confirmation region=us -> 429 Too Many Requests (will retry)"
  fi
  sleep 2
done
