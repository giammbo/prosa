# Staging

This is the staging environment, in the sandbox account: what is written here wins over the intents in the root.
Every resource also has the tag `env=staging`.

## database
- Disposable environment: nothing is left behind on destroy (no final snapshot, no deletion protection).

## app
- The web app's domain is `<DOMAIN>`, in the public Route53 zone with ID `<ROUTE53_ZONE>`.
- The TLS certificate is the already issued ACM one `<CERT_ARN>`.

## cron
- The nightly job's alerts go to `<ALERT_EMAIL>`, through the SNS topic `staging-platform-alerts`.
