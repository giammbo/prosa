#!/usr/bin/env bash
# Demo 1 on the real binary, against Moto: never against real AWS.
#
#   demo/moto/run.sh
#
# Prerequisites: Moto on http://localhost:5555 (docker start prosa-moto, or
# docker compose -f e2e/compose.yaml up -d), go, aws CLI, shasum.
#
# Variables:
#   DEMO_PAUSE=1       wait for Enter after each step (to record the video)
#   DEMO_AUTO=1        answer "yes" to the approval prompts
#   PROSA_PLANNER=llm  use Claude (needs Anthropic credentials); default:
#                      fixtures, that is the simulated states in
#                      demo/fixtures.json. It is a complete run with apply on
#                      Moto. With the real model the assertions of steps 3 and 4
#                      (block on the contradiction, a single change, approval
#                      request) become warnings: the model may not block, or
#                      may change something else as well.
#   PROSA_MOTO_URL     address of Moto (default http://localhost:5555)
#   PROSA_AWS_PROVIDER_VERSION  version of hashicorp/aws written in the initial
#                      lock (default 6.69.1, the tested one)
#   DEMO_DIR           project folder (default: mktemp); if it exists it must
#                      be empty, otherwise the script exits with code 2
#
# The demo project is a copy of examples/ (the intents) and of
# examples/envs/*.md (the environments, ADR-029), without the example locks.
# Moto and the region are given with PROSA_AWS_ENDPOINT and
# PROSA_AWS_REGION=eu-south-1 (no configuration file). The commands run inside
# the folder, as a user would run them. The first step plans the base (`prosa
# plan`, with no environment: the planner writes the full state of every
# intent, ADR-030); the others run on the staging environment (--env
# envs/staging.md), which plans the base once and applies its patch to it.
#
# If the run fails, the emergency cleanup applies with --yes, but only on Moto
# (PROSA_REQUIRE_ENDPOINT=1 refuses to run without an endpoint).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MOTO_URL="${PROSA_MOTO_URL:-http://localhost:5555}"
MOTO_URL="${MOTO_URL%/}"
PLANNER="${PROSA_PLANNER:-fixtures}"
REGION="eu-south-1"
DB_IDENTIFIER="staging-main-db" # identifier of the RDS instance of examples/database.md in staging (simulated states)
ENV_FILE="envs/staging.md"
ENV_ARGS=(--env "$ENV_FILE")
AWS_PROVIDER_VERSION="${PROSA_AWS_PROVIDER_VERSION:-6.69.1}"

die() { echo "demo: $*" >&2; exit 1; }

# --- Never real AWS: the environment is cleaned first ----------------------
for v in $(compgen -e | grep -E '^(AWS_|PROSA_AWS_)' || true); do unset "$v"; done
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_PAGER=""
export AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null
# Endpoint and region: Moto, never real AWS (the AWS region variables were already cleared).
export PROSA_AWS_ENDPOINT="$MOTO_URL" PROSA_AWS_REGION="$REGION"
export PROSA_REQUIRE_ENDPOINT=1

# --- Prerequisites ---------------------------------------------------------
moto_host="${MOTO_URL#*://}"
moto_host="${moto_host%%[/:]*}"
case "$moto_host" in
  localhost | 127.0.0.1) ;;
  *) die "PROSA_MOTO_URL must point to localhost or 127.0.0.1, not to '$moto_host'" ;;
esac
for tool in go curl aws shasum; do
  command -v "$tool" >/dev/null 2>&1 || die "'$tool' is missing from the PATH"
done
case "$PLANNER" in
  fixtures) PLANNER_ARGS=(--fixtures "$ROOT/demo/fixtures.json") ;;
  llm)
    PLANNER_ARGS=()
    if [ -z "${ANTHROPIC_API_KEY:-}" ] && ! { command -v ant >/dev/null 2>&1 && ant auth status >/dev/null 2>&1; }; then
      echo "demo: PROSA_PLANNER=llm needs Anthropic credentials: set ANTHROPIC_API_KEY or sign in with 'ant auth login' (check with 'ant auth status')" >&2
      exit 2
    fi
    ;;
  *) die "PROSA_PLANNER must be 'fixtures' or 'llm', not '$PLANNER'" ;;
esac
if ! curl -fsS --max-time 3 "$MOTO_URL/moto-api/" >/dev/null 2>&1; then
  die "Moto does not answer on $MOTO_URL: start it with 'docker start prosa-moto' or 'docker compose -f e2e/compose.yaml up -d'"
fi

# --- Binary and temporary project ------------------------------------------
echo "== Building bin/prosa"
(cd "$ROOT" && go build -o bin/prosa ./cmd/prosa)
PROSA="$ROOT/bin/prosa"

if [ -n "${DEMO_DIR:-}" ] && [ -n "$(ls -A "$DEMO_DIR" 2>/dev/null)" ]; then
  echo "demo: DEMO_DIR '$DEMO_DIR' is not empty: choose a new or empty folder" >&2
  exit 2
fi
WORK="${DEMO_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/prosa-demo.XXXXXX")}"
mkdir -p "$WORK"
cp "$ROOT"/examples/*.md "$WORK/"
mkdir "$WORK/envs"
cp "$ROOT"/examples/envs/*.md "$WORK/envs/" # the environment files, not the example lock
# The staging lock starts with the tested provider version (the engine picks one itself if it is missing).
cat >"$WORK/envs/staging.lock.yaml" <<LOCK
version: 1
provider_versions:
  hashicorp/aws: $AWS_PROVIDER_VERSION
intents: {}
LOCK
PROSA_STATE_PASSPHRASE="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
export PROSA_STATE_PASSPHRASE
cd "$WORK"

OUT="$WORK/.last-output"
STEP=0

# On failure, best effort: remove what the script created, so Moto is left clean.
on_exit() {
  local rc=$?
  trap - EXIT
  if [ "$rc" -ne 0 ] && [ -f "$WORK/$ENV_FILE" ]; then
    echo "demo: FAILED (code $rc); trying to clean up Moto" >&2
    rm -f "$WORK"/*.md
    (cd "$WORK" && "$PROSA" apply --yes "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"} >/dev/null 2>&1) || true
    echo "demo: project left in $WORK" >&2
  fi
  exit "$rc"
}
trap on_exit EXIT

step() {
  STEP=$((STEP + 1))
  echo
  echo "=== Step $STEP: $*"
  if [ "${DEMO_PAUSE:-}" = "1" ]; then
    read -r -p "(Enter to continue) " _ || true
  fi
}

# prosa runs the binary, shows and records its output (stdout and stderr) in
# $OUT and returns its exit code.
prosa() {
  local shown="$*"
  shown="${shown//" --fixtures $ROOT/demo/fixtures.json"/}" # the path is not valid from the project folder
  echo "\$ prosa $shown"
  local rc=0
  set +e
  "$PROSA" "$@" 2>&1 | tee "$OUT"
  rc=${PIPESTATUS[0]}
  set -e
  return "$rc"
}

# prosa_ok runs prosa and stops the script when it fails.
prosa_ok() {
  local rc=0
  prosa "$@" || rc=$?
  [ "$rc" -eq 0 ] || die "'prosa $*' exited with code $rc"
}

# prosa_fails runs prosa and stops the script when it succeeds.
prosa_fails() {
  local rc=0
  prosa "$@" || rc=$?
  [ "$rc" -ne 0 ] || die "'prosa $*' should have failed but succeeded"
}

# apply runs prosa apply, answering "yes" to the approval when DEMO_AUTO=1.
apply() {
  local rc=0
  if [ "${DEMO_AUTO:-}" = "1" ]; then
    echo "(DEMO_AUTO=1: answering yes)"
    printf 'yes\n' | prosa apply "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"} || rc=$?
  else
    prosa apply "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"} || rc=$?
  fi
  [ "$rc" -eq 0 ] || die "prosa apply exited with code $rc"
}

# SOFT=1 (steps 2 and 3) turns the failed assertions into warnings when the
# planner is the real model, which may not block or may change more than asked.
SOFT=0
soft() { [ "$SOFT" = 1 ] && [ "$PLANNER" = llm ]; }

# expect stops the script unless the last output matches the pattern.
expect() {
  grep -Eq -- "$1" "$OUT" && return 0
  if soft; then
    echo "demo: WARNING: the last output lacks: $1 ($2)" >&2
    return 0
  fi
  die "the last output lacks: $1 ($2)"
}

lock_sha() { shasum -a 256 "$WORK/envs/staging.lock.yaml" | cut -d' ' -f1; }

# state_sha prints one digest over every file of the state store.
state_sha() {
  (cd "$WORK" && find .prosa/state.d/staging -type f | LC_ALL=C sort | xargs shasum -a 256 | shasum -a 256 | cut -d' ' -f1)
}

# aws_show echoes an aws CLI query on Moto and runs it (never fatal).
aws_show() {
  echo "\$ aws --endpoint-url $MOTO_URL --region $REGION $*"
  aws --endpoint-url "$MOTO_URL" --region "$REGION" "$@" || true
}

# edit replaces the only occurrence of $2 with $3 in the intent file $1.
edit() {
  local file="$WORK/$1"
  [ "$(grep -c -F -- "$2" "$file")" = 1 ] || die "$1: '$2' must appear exactly once"
  sed "s/$2/$3/" "$file" >"$file.tmp" && mv "$file.tmp" "$file"
}

if [ "$PLANNER" = fixtures ]; then
  echo "Moto: $MOTO_URL  planner: fixtures (demo/fixtures.json, --fixtures in the commands)  project: $WORK"
else
  echo "Moto: $MOTO_URL  planner: llm (Claude)  project: $WORK"
fi

# ---------------------------------------------------------------------------
step "The base: the plan of the root intents, with no environment"
prosa_ok plan ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"}
expect '^  # [^ ]+ will be created$' "the base plan creates resources"

# ---------------------------------------------------------------------------
step "From the folder to the cloud: plan, apply and checks (staging = base + patch)"
prosa_ok plan "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"}
expect '^  # [^ ]+ will be created$' "the first plan creates resources"
apply
expect '^Apply complete! Resources: ' "apply finished"
echo
echo "--- the resources appeared on Moto"
aws_show rds describe-db-instances --query 'DBInstances[].{Identifier:DBInstanceIdentifier,Status:DBInstanceStatus}' --output table
aws_show ecs list-services --cluster staging-web --query serviceArns --output text
echo
prosa_ok check "${ENV_ARGS[@]}"
expect 'check .*: (pass|skipped) ' "checks run"
if grep -q ': fail ' "$OUT"; then die "a check failed"; fi

# ---------------------------------------------------------------------------
step "One line changes: the check says two replicas, the first sentence still one"
SOFT=1
edit app.md "The number of healthy replicas at rest is the requested one." "There are exactly two healthy replicas at rest."
if soft; then
  prosa plan "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"} || true
else
  prosa_fails plan "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"}
fi
expect '^Blocked:' "the plan stops at the contradiction"
echo
echo "--- ...and I align the first sentence too: only one thing changes"
edit app.md "with one replica" "with two replicas"
prosa_ok plan "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"}
# One block per change, each under its "# <intent>/<key> will be ..." header.
changes="$(grep -Ec '^  # [^ ]+ (will be|must be) ' "$OUT" || true)"
if [ "$changes" != 1 ]; then
  if soft; then
    echo "demo: WARNING: the plan has $changes changes, one was expected" >&2
  else
    die "the plan must have a single change, it has $changes"
  fi
fi
expect '^  # app/[^ ]+ will be updated in-place$' "the change is an update..."
expect '^  ~ resource "aws_ecs_service" ' "...of the ECS service"
expect '~ desired_count += 1 -> 2$' "a single update of the service"
apply
expect '^Apply complete! Resources: 0 added, 1 changed, 0 destroyed\.' "apply finished"
SOFT=0

# ---------------------------------------------------------------------------
step "Delete cron.md: its resources go away, with approval"
SOFT=1
rm "$WORK/cron.md"
# Its section in the environment file too: after this run "## cron" would no longer name an intent and would give a warning on stderr: it is removed to avoid that.
awk '/^## /{skip = ($0 == "## cron")} !skip' "$WORK/$ENV_FILE" >"$WORK/$ENV_FILE.tmp" && mv "$WORK/$ENV_FILE.tmp" "$WORK/$ENV_FILE"
prosa_ok plan "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"}
expect '# \(needs approval: destroy\)' "the destruction asks for approval"
apply
expect '^Apply complete! Resources: 0 added, 0 changed, [1-9][0-9]* destroyed\.' "apply finished"
SOFT=0

# ---------------------------------------------------------------------------
step "Three reruns in a row: the cloud does not move, lock and state store are identical"
lock_before="$(lock_sha)"
state_before="$(state_sha)"
for i in 1 2 3; do
  echo "--- rerun $i"
  # Nothing to do: apply neither asks for approval (stdin is closed) nor writes.
  prosa_ok apply "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"} </dev/null
  expect '^No changes\.' "rerun $i without changes"
done
echo "lock before: $lock_before"
echo "lock after:  $(lock_sha)"
echo "state before: $state_before"
echo "state after:  $(state_sha)"
[ "$lock_before" = "$(lock_sha)" ] || die "the lock changed between the reruns"
[ "$state_before" = "$(state_sha)" ] || die "the state store changed between the reruns"
echo "lock identical."
echo "state store identical."

# ---------------------------------------------------------------------------
step "A manual change (aws CLI, on Moto): the engine sees it and restores it"
echo "\$ aws --endpoint-url $MOTO_URL --region $REGION rds modify-db-instance --db-instance-identifier $DB_IDENTIFIER --allocated-storage 50 --apply-immediately"
aws --endpoint-url "$MOTO_URL" --region "$REGION" rds modify-db-instance \
  --db-instance-identifier "$DB_IDENTIFIER" --allocated-storage 50 --apply-immediately \
  --output text --query 'DBInstance.DBInstanceIdentifier'
prosa_ok plan "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"}
expect '# \(drifted outside Prosa: allocated_storage\)' "the drift is reported"
apply
expect '^Apply complete! Resources: 0 added, 1 changed, 0 destroyed\.' "apply finished"
prosa_ok plan "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"}
expect '^No changes\.' "after the restore there is nothing left to do"

# ---------------------------------------------------------------------------
step "Cleanup: remove all the intents and apply"
rm -f "$WORK"/*.md
prosa_ok plan "${ENV_ARGS[@]}" ${PLANNER_ARGS[@]+"${PLANNER_ARGS[@]}"}
apply
expect '^Apply complete! Resources: 0 added, 0 changed, [1-9][0-9]* destroyed\.' "apply finished"

echo
[ -s "$WORK/.prosa/events/staging.jsonl" ] || die "the event log $WORK/.prosa/events/staging.jsonl does not exist"
echo "=== Done. Event log: $WORK/.prosa/events/staging.jsonl"
