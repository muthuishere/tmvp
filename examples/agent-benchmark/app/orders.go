package app

// Repository queries (evidence for discovery).
const (
	qStuckOrders   = `select id, status, created_at from orders where status = 'pending_payment' and created_at < now() - interval '15 minutes'`
	qWebhookErrors = `select status_code, error, count(*) from webhook_events where received_at > now() - interval '1 hour' group by 1,2`
	qConfig        = `select key, value from app_config where key like 'payfast.%'`
)

// DATABASE_URL is read from the environment; PAYFAST_WEBHOOK_SECRET signs webhooks.
