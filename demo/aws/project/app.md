---
autonomy: approve
depends_on: [database]
---
# Web app

I want the web app from the image `<IMAGE>`, which listens on port 80 and answers 200 on `/health`,
exposed at the environment's domain with a valid TLS certificate, with two replicas. It uses the database from `database.md`:
the connection string is passed as the environment variable `DATABASE_URL`,
read from the secret, never in plain text.

It must scale up to four replicas if the average CPU stays above 70% for five minutes.

The TLS certificate, the domain and the Route53 zone are the environment's: the certificate has already been issued and
Prosa neither creates nor deletes it. In the environment's public zone create an alias record for the domain pointing to the
load balancer. The load balancer answers HTTPS on port 443 and redirects port 80 to HTTPS.

## Constraints
- Containers run in the private subnets of the network in `database.md` (no second VPC and no NAT),
  behind a public load balancer in the public subnets of the same network.
- Required tag: `team=platform`.

## Checks
- `https://<the environment's domain>/health` answers 200 within 5 minutes of the end of the apply.
- The certificate is valid for more than 20 days.
- There are exactly two healthy replicas at rest.
