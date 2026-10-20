---
autonomy: approve
depends_on: [database]
---
# Web app

I want the web app from the image `ghcr.io/acme/web:1.4.2`, exposed at `app.acme.example`
with a valid TLS certificate, with one replica. It uses the database from `database.md`:
the connection string is passed as the environment variable `DATABASE_URL`,
read from the secret, never in plain text.

## Constraints
- Containers run in private subnets, behind a public load balancer.
- Required tag: `team=platform`.

## Checks
- `/health` on the web app's domain answers 200 within 60 seconds of the end of the apply.
- The certificate is valid for more than 20 days.
- The number of healthy replicas at rest is the requested one.
