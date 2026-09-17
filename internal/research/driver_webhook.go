package research

// driver_webhook.go — the relayed driver (SPEC-07 §2.2).
//
// Structurally identical to the off-by-one driver: the same request and response
// shapes and the same four surfaces, with `research.webhook_url` as the POST
// target and `research.webhook_poll_url` as the GET target
// (`?submission_id=`). It exists so an operator running their own lab behind a
// gateway gets identical outcome semantics and degrade paths; it exists as a
// separate driver because a relayed lab is a different trust and capability
// domain, not a URL change.

import "github.com/totalwindupflightsystems/trouble/internal/types"

// newWebhook returns the gateway-relayed driver. A missing webhook URL is
// refused at config validation (SPEC-07 §4.3): a webhook driver with nowhere to
// POST is a driver that silently does nothing.
func newWebhook(cfg config) *offByOne {
	d := newHTTPDriver(cfg)
	d.name = types.DriverWebhook
	d.webhook = true
	d.base = "" // the webhook URLs are absolute by configuration
	d.discoverPath = cfg.WebhookURL
	d.submitPath = cfg.WebhookURL
	d.healthPath = cfg.WebhookURL
	d.statsPath = cfg.WebhookURL
	d.openAPIPath = cfg.WebhookURL
	if cfg.WebhookPollURL != "" {
		d.queuePath = cfg.WebhookPollURL
	}
	return d
}
