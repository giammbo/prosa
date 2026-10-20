#!/usr/bin/env bash
# Preflight for the Prosa run on real AWS.
#
# READ-ONLY: it only calls AWS get/describe/list operations and creates,
# changes or deletes nothing. The user runs it, with the credentials of the
# sandbox account already active (for example AWS_PROFILE).
#
# Usage: demo/aws/preflight.sh [project folder [environment file]]
#        (defaults: $HOME/prosa-aws and envs/staging.md inside the project)
# Reads: the project (the intents app.md, database.md, cron.md and the environment file, ADR-029),
#        DEMO_DOMAIN, DEMO_ZONE_ID, PROSA_STATE_PASSPHRASE, ANTHROPIC_API_KEY
#        (or an `ant auth login` profile), the region (PROSA_AWS_REGION, AWS_REGION,
#        AWS_DEFAULT_REGION or the profile, as prosa does).
# Prosa does not check the account (ADR-029): here the identity of the active credentials is read
# and printed, so that the user compares it with the sandbox account.
# The certificate is read from the ARN written in the environment file
# (in place of <CERT_ARN>; the certificate is issued at step 3).
# Prints one OK / FAIL / WARN line per check; exits with 1 if there is at least one FAIL.

set -u
export AWS_PAGER=""

PROJECT_DIR="${1:-$HOME/prosa-aws}"
ENV_FILE="${2:-$PROJECT_DIR/envs/staging.md}"
FAILED=0

# The global APIs (STS, Account, Route53) are called on us-east-1, whatever the region.
GLOBAL_REGION=us-east-1

ok()     { printf 'OK      %s\n' "$*"; }
ko()     { printf 'FAIL    %s\n' "$*"; FAILED=1; }
warn() { printf 'WARN    %s\n' "$*"; }
lower()  { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# --- Project (intents and environment file) --------------------------------

if [ ! -d "$PROJECT_DIR" ]; then
  ko "project folder $PROJECT_DIR is missing"
else
  ok "project: $PROJECT_DIR"
fi
if [ -f "$PROJECT_DIR/prosa.yaml" ]; then
  ko "$PROJECT_DIR/prosa.yaml exists: ADR-029 no longer uses it and prosa rejects the folder (delete it)"
fi
if [ -d "$PROJECT_DIR/intents" ]; then
  warn "$PROJECT_DIR/intents exists: it is the layout from before ADR-027, prosa only reads the .md files in the root"
fi
if [ ! -f "$ENV_FILE" ]; then
  ko "environment file $ENV_FILE is missing"
else
  case "$(cd "$(dirname "$ENV_FILE")" && pwd)" in
    "$(cd "$PROJECT_DIR" 2>/dev/null && pwd)") ko "$ENV_FILE is in the project root, where every .md is an intent: move it to envs/" ;;
    *) ok "environment: $ENV_FILE" ;;
  esac
fi

# The region is prosa's: PROSA_AWS_REGION, AWS_REGION, AWS_DEFAULT_REGION, then the profile.
REGION="${PROSA_AWS_REGION:-${AWS_REGION:-${AWS_DEFAULT_REGION:-}}}"
if [ -z "$REGION" ] && command -v aws >/dev/null 2>&1; then
  REGION=$(aws configure get region 2>/dev/null || true)
fi
if [ -z "$REGION" ]; then
  ko "no region: set AWS_REGION (or PROSA_AWS_REGION) or the region of the AWS profile"
fi

# --- Local environment -----------------------------------------------------

if [ -n "${PROSA_AWS_ENDPOINT:-}" ]; then
  ko "PROSA_AWS_ENDPOINT=$PROSA_AWS_ENDPOINT is set: prosa would point to the emulator (unset PROSA_AWS_ENDPOINT)"
else
  ok "PROSA_AWS_ENDPOINT is not set: prosa points to real AWS"
fi

if [ "${PROSA_REQUIRE_ENDPOINT:-}" = "1" ]; then
  ko "PROSA_REQUIRE_ENDPOINT=1: prosa would refuse to start without an emulator (unset PROSA_REQUIRE_ENDPOINT)"
else
  ok "PROSA_REQUIRE_ENDPOINT is not active"
fi

stray=$(env | cut -d= -f1 | grep -E '^AWS_ENDPOINT_URL(_|$)' | tr '\n' ' ')
if [ -n "$stray" ]; then
  ko "AWS endpoint variables are set ($stray): the aws commands would go elsewhere (unset $stray)"
else
  ok "no AWS_ENDPOINT_URL* variable"
fi

if [ -z "$REGION" ]; then
  : # already reported above
elif [ "$REGION" = "eu-south-1" ]; then
  ok "region: eu-south-1"
else
  ko "region $REGION (AWS_REGION, PROSA_AWS_REGION or profile): the intents ask for eu-south-1 and the policy rejects other regions"
fi

if [ -n "${PROSA_STATE_PASSPHRASE:-}" ]; then
  if [ "${#PROSA_STATE_PASSPHRASE}" -lt 16 ]; then
    warn "PROSA_STATE_PASSPHRASE is short (${#PROSA_STATE_PASSPHRASE} characters): use a long one and keep it off this machine"
  else
    ok "PROSA_STATE_PASSPHRASE is set (saved in the password manager? without it, every resource is orphaned)"
  fi
else
  ko "PROSA_STATE_PASSPHRASE is not set: it encrypts the state store"
fi

if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  ok "Anthropic credentials: ANTHROPIC_API_KEY is set"
elif command -v ant >/dev/null 2>&1 && ant auth status >/dev/null 2>&1; then
  ok "Anthropic credentials: ant auth login profile"
else
  ko "Anthropic credentials missing: export ANTHROPIC_API_KEY=... or run ant auth login"
fi

if [ -d "$PROJECT_DIR" ]; then
  missing=""
  for f in database.md app.md cron.md; do
    [ -f "$PROJECT_DIR/$f" ] || missing="$missing $f"
  done
  if [ -n "$missing" ]; then
    ko "missing in $PROJECT_DIR:$missing"
  else
    ok "intents present in $PROJECT_DIR"
  fi
  left=$(grep -lE '<(DOMAIN|ROUTE53_ZONE|IMAGE|CERT_ARN|ALERT_EMAIL)>' "$PROJECT_DIR"/*.md "$ENV_FILE" 2>/dev/null | tr '\n' ' ')
  if [ -n "$left" ]; then
    ko "placeholders not replaced in: $left"
  else
    ok "no placeholder left in the intents and in the environment"
  fi
fi

if command -v prosa >/dev/null 2>&1; then
  ok "prosa in the PATH: $(command -v prosa)"
else
  ko "prosa is not in the PATH: build it and add bin/ to the PATH"
fi
if command -v jq >/dev/null 2>&1; then
  ok "jq present"
else
  ko "jq not found: it is used to read events and the planner cache"
fi
if command -v docker >/dev/null 2>&1; then
  ok "docker present"
else
  ko "docker not found: it is needed to try the image and for the dress rehearsal on Moto"
fi

if ! command -v aws >/dev/null 2>&1; then
  ko "aws CLI not found: the AWS checks cannot be done"
  echo
  echo "Preflight failed."
  exit 1
fi
case "$(aws --version 2>&1)" in
  aws-cli/2.*) ok "aws CLI v2" ;;
  *) warn "aws CLI is not v2 ($(aws --version 2>&1)): some commands may be missing" ;;
esac

# --- AWS (read-only) -------------------------------------------------------

if ident=$(aws sts get-caller-identity --region "$GLOBAL_REGION" --query '[Account,Arn]' --output text 2>&1); then
  ok "AWS identity: $ident"
  warn "prosa does not check the account: compare the identity above by hand with the sandbox account you mean to use"
else
  ko "aws sts get-caller-identity failed: $ident"
  echo
  echo "Preflight failed: without AWS credentials the other checks make no sense."
  exit 1
fi

if opt=$(aws account get-region-opt-status --region "$GLOBAL_REGION" --region-name "$REGION" --query RegionOptStatus --output text 2>&1); then
  case "$opt" in
    ENABLED|ENABLED_BY_DEFAULT) ok "region $REGION enabled ($opt)" ;;
    ENABLING) ko "region $REGION is being enabled: try again later" ;;
    *) ko "region $REGION is not enabled ($opt): aws account enable-region --region-name $REGION (the user runs it)" ;;
  esac
else
  ko "status of region $REGION not readable: $opt"
fi

if [ -z "${DEMO_ZONE_ID:-}" ] || [ -z "${DEMO_DOMAIN:-}" ]; then
  ko "DEMO_ZONE_ID and DEMO_DOMAIN must be set"
elif zone=$(aws route53 get-hosted-zone --region "$GLOBAL_REGION" --id "$DEMO_ZONE_ID" --query '[HostedZone.Name,HostedZone.Config.PrivateZone]' --output text 2>&1); then
  zone_name=$(lower "$(printf '%s' "$zone" | cut -f1)")
  zone_name=${zone_name%.}
  private=$(lower "$(printf '%s' "$zone" | cut -f2)")
  domain=$(lower "${DEMO_DOMAIN%.}")
  if [ "$private" = "true" ]; then
    ko "zone $DEMO_ZONE_ID ($zone_name) is private: ACM only validates on a public zone"
  elif [ "$domain" != "$zone_name" ] && [ "${domain%".$zone_name"}" = "$domain" ]; then
    ko "$domain is not in zone $zone_name ($DEMO_ZONE_ID)"
  else
    ok "Route53 zone $DEMO_ZONE_ID ($zone_name) is public and contains $domain"
    want=$(aws route53 get-hosted-zone --region "$GLOBAL_REGION" --id "$DEMO_ZONE_ID" --query 'DelegationSet.NameServers' --output text 2>/dev/null \
      | tr '\t' '\n' | tr '[:upper:]' '[:lower:]' | sed 's/\.$//' | sort | tr '\n' ' ')
    if ! command -v dig >/dev/null 2>&1; then
      warn "dig not found: DNS delegation of the zone not verified"
    else
      got=$(dig +short NS "$zone_name" | tr '[:upper:]' '[:lower:]' | sed 's/\.$//' | sort | tr '\n' ' ')
      if [ -n "$got" ] && [ "$got" = "$want" ]; then
        ok "DNS delegation of $zone_name points to the zone's nameservers"
      else
        ko "DNS delegation of $zone_name: public [${got:-none}], zone's [$want]; without delegation ACM does not issue the certificate"
      fi
    fi
  fi
else
  ko "Route53 zone $DEMO_ZONE_ID not readable: $zone"
fi

# quota <service-code> <quota-code> <name>: value of the quota in the region;
# if it is not readable it says so with a WARN (on stderr, outside the capture) and is 5, the default.
quota() {
  local v
  v=$(aws service-quotas get-service-quota --region "$REGION" --service-code "$1" --quota-code "$2" \
    --query Quota.Value --output text 2>/dev/null) || v=""
  case "$v" in
    ''|None)
      warn "quota $3 ($2) not readable: using the default value 5" >&2
      echo 5 ;;
    *) printf '%.0f\n' "$v" ;;
  esac
}

if used=$(aws ec2 describe-vpcs --region "$REGION" --query 'length(Vpcs)' --output text 2>&1); then
  limit=$(quota vpc L-F678F1CE "VPCs per region")
  if [ "$used" -lt "$limit" ]; then
    ok "VPCs in $REGION: $used of $limit (1 needed)"
  else
    ko "VPCs in $REGION: $used of $limit, none free (1 needed)"
  fi
else
  ko "VPCs not readable: $used"
fi

if used=$(aws ec2 describe-addresses --region "$REGION" --query 'length(Addresses)' --output text 2>&1); then
  limit=$(quota ec2 L-0263D0A3 "Elastic IP")
  if [ "$used" -lt "$limit" ]; then
    ok "Elastic IPs in $REGION: $used of $limit (1 needed, for the NAT)"
  else
    ko "Elastic IPs in $REGION: $used of $limit, none free (1 needed, for the NAT)"
  fi
else
  ko "Elastic IPs not readable: $used"
fi

if left=$(aws resourcegroupstaggingapi get-resources --region "$REGION" --tag-filters Key=prosa/intent \
  --query 'length(ResourceTagMappingList)' --output text 2>&1); then
  if [ "$left" = "0" ]; then
    ok "no resource with the prosa/intent tag in $REGION"
  else
    warn "$left resources with the prosa/intent tag in $REGION: leftovers of a previous run?"
  fi
else
  warn "resources with the prosa/intent tag not readable: $left"
fi

cert=""
[ -f "$ENV_FILE" ] && cert=$(grep -oE 'arn:aws:acm:[a-z0-9-]+:[0-9]+:certificate/[A-Za-z0-9-]+' "$ENV_FILE" | head -1)
if [ -z "$cert" ]; then
  warn "no ACM certificate ARN in the environment file: this is the experiment of DNS validation in a single apply (04, #30)"
elif info=$(aws acm describe-certificate --region "$REGION" --certificate-arn "$cert" \
  --query 'Certificate.[Status,DomainName]' --output text 2>&1); then
  status=$(printf '%s' "$info" | cut -f1)
  cert_domain=$(lower "$(printf '%s' "$info" | cut -f2)")
  if [ "$status" != "ISSUED" ]; then
    ko "certificate $cert is in state $status: ISSUED is needed"
  elif [ -n "${DEMO_DOMAIN:-}" ] && [ "$cert_domain" != "$(lower "${DEMO_DOMAIN%.}")" ]; then
    ko "certificate $cert issued for $cert_domain, not for $DEMO_DOMAIN"
  else
    ok "ACM certificate issued for $cert_domain"
  fi
else
  ko "certificate $cert not readable in $REGION: $info"
fi

echo
if [ "$FAILED" -ne 0 ]; then
  echo "Preflight failed: fix the FAIL lines and run it again."
  exit 1
fi
echo "Preflight passed."
