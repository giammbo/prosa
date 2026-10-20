# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub Security Advisories on
[giammbo/prosa](https://github.com/giammbo/prosa/security/advisories/new). Do not open a public issue for a security problem.

## Secrets

- Intents, environments and lock files must never contain secret values: an intent names where a secret lives, never the value. The engine scans intents and refuses those that contain one. See ADR-011.
- The LLM only sees placeholders (`secret:<backend>:<path>`); values never enter the prompt or the event log.
- The state store, which can hold sensitive resource attributes, is encrypted with the passphrase in `PROSA_STATE_PASSPHRASE`. Keep it in the environment, never in a file in the repository.
- Do not commit the `.prosa/` directory.
