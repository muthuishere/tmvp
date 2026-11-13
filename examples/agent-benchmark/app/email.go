package app

// Mailer config comes from app_config (email.<region>.region, email.from,
// email.<region>.sender_domain). A send that fails stays in email_outbox
// with status 'retrying' and is retried up to 5 times.
const (
	qMailerConfig = `select key, value from app_config where key like 'email.%'`
	qOutboxStuck  = `select region, status, error, count(*) from email_outbox where status <> 'sent' group by 1,2,3`
)

// regionEndpoint builds https://email.<region>.provider.example — the region
// value is used exactly as stored.
