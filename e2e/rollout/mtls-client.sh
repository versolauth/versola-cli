#!/usr/bin/env bash
# Makes the mutual-TLS client the rollout e2e's --real run sends to auth's 8083:
# a service client registered in central, an EST client certificate for it, and
# one /token over 8083 to prove both work before any load starts. Written from
# versola's scripts/est-smoke.sh (steps 1-4), which an operator runs against a
# stand by hand; read its header for what each step is.
#
#   mtls-client.sh OUT_DIR
#
# Writes OUT_DIR/client.crt, OUT_DIR/client.key and OUT_DIR/client.id (the
# client's id). Settings, all from the environment (defaults are the e2e's):
#   E2E_CENTRAL_URL   central's admin API, the proxy's front  (http://127.0.0.1:8090)
#   E2E_AUTH_URL      auth's public origin through the proxy (http://127.0.0.1:2821)
#   E2E_AUTH_HOST     Host header for it                       (e2e.test)
#   E2E_MTLS_ADDR     auth's mutual-TLS pass-through          (127.0.0.1:8083)
#   E2E_SECRETS_ENV   the deployment's central.secrets.env: REQUIRED. The admin
#                     API takes the CENTRAL_RESOURCE_SECRET in it as the HTTP
#                     Basic password of the user `central`, the way
#                     `versola edge add` reaches it (internal/deploy/edge.go).
#   E2E_TENANT        tenant the client is made in            (default)
#   E2E_SCOPE         scope it may be granted                 (openid)
#   E2E_SYNC_WAIT_SECONDS  how long to wait for auth to learn of the client (420)
#
# The secret is read into a shell variable, handed to curl on its stdin and
# never printed, put on a command line or written anywhere. No token, key or
# response body is printed either: status lines only, and when a step is refused the
# first 300 bytes of the error answer (central's errors do not echo the request).
# The client is not deleted:
# it lives in the e2e's own throwaway deployment, which `run.sh clean` removes
# with its database. Needs bash, curl, openssl and python3, like est-smoke.sh.
set -euo pipefail
umask 077
# A trace (bash -x) would print the admin secret: never trace this script.
{ set +x; } 2>/dev/null

fail() { echo "e2e mtls: $*" >&2; exit 1; }
note() { echo "e2e mtls: $*"; }

OUT="${1:-}"
[ -n "$OUT" ] || fail "usage: mtls-client.sh OUT_DIR"
CENTRAL="${E2E_CENTRAL_URL:-http://127.0.0.1:8090}"
AUTH="${E2E_AUTH_URL:-http://127.0.0.1:2821}"
AUTH_HOST="${E2E_AUTH_HOST:-e2e.test}"
MTLS="${E2E_MTLS_ADDR:-127.0.0.1:8083}"
TENANT="${E2E_TENANT:-default}"
SCOPE="${E2E_SCOPE:-openid}"
SECRETS_ENV="${E2E_SECRETS_ENV:-}"
[ -n "$SECRETS_ENV" ] || fail "E2E_SECRETS_ENV (the deployment's central.secrets.env) is required"
[ -r "$SECRETS_ENV" ] || fail "cannot read the deployment's central.secrets.env"
for tool in curl openssl python3; do
  command -v "$tool" >/dev/null || fail "$tool is not installed"
done

# KEY=value or KEY="value" (as Compose reads it), value never printed.
SECRET=""
while IFS= read -r line; do
  case "$line" in
    CENTRAL_RESOURCE_SECRET=*)
      SECRET="${line#CENTRAL_RESOURCE_SECRET=}"
      case "$SECRET" in \"*\") SECRET="${SECRET#\"}"; SECRET="${SECRET%\"}" ;; \'*\') SECRET="${SECRET#\'}"; SECRET="${SECRET%\'}" ;; esac
      break ;;
  esac
done <"$SECRETS_ENV"
[ -n "$SECRET" ] || fail "central.secrets.env has no CENTRAL_RESOURCE_SECRET"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$OUT"
CLIENT_ID="e2e-mtls-$(openssl rand -hex 4)"

# The admin credential goes in on stdin: in no process list, no log.
curl_central() {
  printf 'user = "central:%s"\n' "$SECRET" | curl -sS --max-time 20 --config - "$@"
}
# The start of an error answer, for the failure messages (never of a success).
why() { head -c 300 "$1" 2>/dev/null | tr -c "[:print:]" " "; }
json_get() { python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$1" "$2"; }

# 1. register a service client that authenticates with its certificate
python3 - "$CLIENT_ID" "$TENANT" "$SCOPE" >"$work/register.json" <<'PY'
import json, sys
print(json.dumps({
    "tenantId": sys.argv[2], "id": sys.argv[1], "clientName": {"en": "rollout e2e mutual TLS"},
    "allowedScopes": [sys.argv[3]], "permissions": [], "accessTokenTtl": 300,
    "dpopBoundAccessTokens": False, "dpopSigningAlgs": [], "authMethod": "tls_client_auth",
    "certificateBoundAccessTokens": True, "estEnrollment": True,
}))
PY
code="$(curl_central -X POST "$CENTRAL/configuration/clients/service" -H 'Content-Type: application/json' \
  --data @"$work/register.json" -o "$work/register.out" -w '%{http_code}')" || code=error
[ "$code" = 201 ] || fail "registering the client answered HTTP $code: $(why "$work/register.out") (the tenant $TENANT must exist, its settings must allow an EST client -- mtlsCertificateHeader --, and the admin secret must be central's)"
note "1. registered $CLIENT_ID (HTTP $code)"

# 2. a one-time enrolment token, and the subject the certificate must carry
code="$(curl_central -X POST "$CENTRAL/configuration/clients/est/enrollment-tokens?clientId=$CLIENT_ID" -o "$work/token.json" -w '%{http_code}')" || code=error
[ "$code" = 201 ] || fail "creating the enrolment token answered HTTP $code: $(why "$work/token.json")"
SUBJECT="$(json_get "$work/token.json" subject)"
[ -n "$SUBJECT" ] || fail "no subject in the enrolment token answer"
note "2. enrolment token created (HTTP $code)"

# openssl's -subj lists the RDNs in encoding order, the reverse of RFC 4514's.
SUBJ="$(python3 -c 'import sys; print("".join("/" + p for p in reversed(sys.argv[1].split(","))))' "$SUBJECT")"
openssl ecparam -name prime256v1 -genkey -noout -out "$work/k.ec.pem" 2>/dev/null
openssl pkcs8 -topk8 -nocrypt -in "$work/k.ec.pem" -out "$work/client.key" 2>/dev/null
openssl req -new -key "$work/k.ec.pem" -subj "$SUBJ" -outform DER 2>/dev/null | openssl base64 -e >"$work/k.csr.b64"

# 3. enrol over auth's public origin (the proxy routes /.well-known/ to auth)
code="$(curl -sS --max-time 20 -X POST "$AUTH/.well-known/est/simpleenroll" -H "Host: $AUTH_HOST" -H 'Content-Type: application/pkcs10' \
  --config <(printf 'user = "%s:%s"\n' "$CLIENT_ID" "$(json_get "$work/token.json" token)") \
  --data-binary @"$work/k.csr.b64" -o "$work/enrol.b64" -w '%{http_code}')" || code=error
[ "$code" = 200 ] || fail "simpleenroll answered HTTP $code: $(why "$work/enrol.b64") (an empty 404 means EST is off in auth: the deployment needs \`configure --est\` and a versola-tools of 0.8.0 or later)"
# The leaf of a certs-only PKCS#7 answer, whatever order it lists its certificates in.
openssl base64 -d -in "$work/enrol.b64" -out "$work/enrol.der" 2>/dev/null || fail "the enrolment answer is not base64"
openssl pkcs7 -inform DER -in "$work/enrol.der" -print_certs 2>/dev/null >"$work/enrol.all" || fail "the enrolment answer is not a PKCS#7"
python3 - "$work/enrol.all" "$CLIENT_ID" "$work/client.crt" <<'PY' || fail "no certificate for the client in the enrolment answer"
import re, subprocess, sys
text = open(sys.argv[1]).read()
for pem in re.findall(r"-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----", text, re.S):
    subject = subprocess.run(["openssl", "x509", "-noout", "-subject"], input=pem, capture_output=True, text=True).stdout
    if "CN=" + sys.argv[2] in subject.replace(" ", "") or "CN = " + sys.argv[2] in subject:
        open(sys.argv[3], "w").write(pem + "\n")
        sys.exit(0)
sys.exit(1)
PY
note "3. enrolled (HTTP $code)"

# 4. /token over 8083 with the certificate. auth learns of a client by a sync of
# its cache, every five minutes by default (README.md "Short outages"), so it may
# answer 401 invalid_client for that long after the registration (est-smoke.sh
# retries 10 x 3 s, enough only against a stand with a short refresh). Polled
# every 5 s for E2E_SYNC_WAIT_SECONDS (default 420).
SYNC_WAIT="${E2E_SYNC_WAIT_SECONDS:-420}"
[[ "$SYNC_WAIT" =~ ^[0-9]+$ ]] || fail "E2E_SYNC_WAIT_SECONDS must be a number of seconds"
code=000
waited=0
while :; do
  code="$(curl -sSk --max-time 20 --cert "$work/client.crt" --key "$work/client.key" -X POST "https://$MTLS/token" \
    --data-urlencode grant_type=client_credentials --data-urlencode "client_id=$CLIENT_ID" --data-urlencode "scope=$SCOPE" \
    -o "$work/token.out" -w '%{http_code}' 2>/dev/null)" || code=error
  [ "$code" = 200 ] && break
  [ "$waited" -lt "$SYNC_WAIT" ] || break
  [ "$waited" -ne 30 ] || note "4. auth does not know the client yet (HTTP $code); waiting for its cache refresh, up to ${SYNC_WAIT} s"
  sleep 5
  waited=$((waited + 5))
done
[ "$code" != 200 ] || note "4. auth knew the client after ${waited} s"
[ "$code" = 200 ] || fail "/token over $MTLS with the certificate answered HTTP $code: $(why "$work/token.out")"
note "4. /token over $MTLS with the certificate (HTTP $code)"

cp "$work/client.crt" "$OUT/client.crt"
cp "$work/client.key" "$OUT/client.key"
printf '%s\n' "$CLIENT_ID" >"$OUT/client.id"
note "client certificate and key written to $OUT"
