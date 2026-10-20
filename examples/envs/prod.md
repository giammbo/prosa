# Production

This is the production environment: what is written here wins over the intents in the root.
Every resource also has the tag `env=prod`.

## database
- Multi-AZ, with 100 GB of storage and backups kept for 30 days.

## app
- The web app is exposed at `www.acme.example` with two replicas, which scale up to four
  if the average CPU stays above 70% for five minutes.
