# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project aims to follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it reaches 1.0.

## [Unreleased]

### Changed
- Prosa now names its category **Infrastructure as Prompt** (it was "Infrastructure as Intent").

## [0.2.0] — 2026-10-20

### Added
- First run on real AWS (eu-west-1): create at the smallest sizes, resize, scale and destroy, with Claude as the planner.

### Changed
- `prosa apply` now always asks for confirmation before any change, like Terraform (`Do you want to perform these actions? Only 'yes' will be accepted:`), including for `autonomy: auto` intents; `--yes` skips it (ADR-033).

## [0.1.0] — 2026-10-20

First public release. Tested against Moto (an AWS emulator); the run on real AWS is prepared but not executed yet.

### Added
- Provider host: Terraform/OpenTofu providers used as drivers over gRPC (plugin protocol 5), downloaded from the registry with verification; no HCL.
- Engine: a folder of Markdown intents is the desired state; `prosa plan` and `prosa apply` reconcile it against the provider APIs. Plans are mandatory and destruction asks for approval.
- Lockfile that freezes the concrete choices (names, IDs, provider versions) and a client-side encrypted state store (`PROSA_STATE_PASSPHRASE`).
- Drift detection: changes made outside Prosa are reported in the plan and restored on apply.
- Checks written in the prompt (`resource_attr`, `http_get`, `tls_days`) run with `prosa check`.
- Policy and autonomy levels declared in the prompt, templates and `depends_on`, an append-only event log (`.prosa/events/`) and `prosa recover`.
- Claude planner: a streaming tool-use loop that reads the provider schemas, with a cache in `.prosa/planner/`; a second backend for any OpenAI-compatible endpoint (`PROSA_LLM=openai`, local models included).
- Environments as prose files passed with `--env` (`envs/<name>.md`), with `## <intent>` sections; per-environment lock, state and events; region and credentials from the AWS CLI configuration.
- The base is planned once and an environment is a patch on top of it (`submit_changes`), so an environment costs few tokens; model and effort are settable per job.
- Project as a folder (`prosa init`), `prosa version`, and `prosa debug schema` to inspect a provider schema.
- Moto demo (`demo/moto/run.sh`) and end-to-end tests (`PROSA_E2E=1 go test ./e2e/`), plus a runbook and preflight for a run on real AWS.
- Release archives for linux and darwin (amd64, arm64) with checksums, and `install.sh` to install the binary without cloning the repo.
