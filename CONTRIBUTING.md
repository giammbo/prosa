# Contributing to Prosa

Thanks for helping. Prosa is early; small, verifiable steps are preferred over large changes.

## Prerequisites

- Go (the version in `go.mod`).
- Docker, only for the end-to-end tests: they run against [Moto](https://github.com/getmoto/moto), an AWS API mock.
- The `aws` CLI, only for the end-to-end tests (the drift step and the verifications use it).

## Build

```sh
go build ./...
```

## Offline tests

These need no credentials and no network:

```sh
go test ./... && go vet ./...
```

Tests must never call real AWS or a real LLM. Planner conversations are simulated (see below).

## End-to-end tests on Moto

```sh
docker compose -f e2e/compose.yaml up -d      # or: docker start prosa-moto
PROSA_E2E=1 go test ./e2e/ -count=1 -timeout 50m
```

Moto is expected at `http://localhost:5555` (override with `PROSA_MOTO_URL`). The run downloads the AWS provider from the OpenTofu registry.

The demo script can be run by hand: `demo/moto/run.sh` (`DEMO_AUTO`, `DEMO_PAUSE`, `PROSA_PLANNER=llm`).

## Regenerating simulated data

After any change to the planner prompt, the tools, or the simulated states, regenerate the recorded conversations:

```sh
PROSA_LLM_SIMULATE=1 go test ./e2e -run TestWriteSimulatedConversations
```

The demo fixtures (`demo/fixtures.json`) are regenerated with:

```sh
PROSA_DEMO_FIXTURES=1 go test ./e2e -run TestWriteDemoFixtures
```

## Design decisions

Design decisions are discussed in an issue before a PR. ADR numbers cited in code comments are internal references to the design notes, which are maintained privately.

## Examples are the user contract

`examples/` shows the format of intents, environments and locks. If the format changes, update the examples first, then the code.

## Pull requests

Keep them focused, with tests, and make sure the offline checks pass. By contributing you agree that your work is licensed under the Apache License 2.0. Files under `internal/tfplugin5` keep their MPL-2.0 headers (see `NOTICE`).
