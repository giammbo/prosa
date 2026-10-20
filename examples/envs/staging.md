# Staging

This is the staging environment: what is written here wins over the intents in the root.
Every resource also has the tag `env=staging`.

## database
- 40 GB of storage and Postgres 18.

## app
- The web app is exposed at `staging.acme.example`.

## cron
- The nightly job does not send email alerts.
