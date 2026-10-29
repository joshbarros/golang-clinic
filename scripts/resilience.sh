#!/usr/bin/env bash
# What Kubernetes buys: kill an API replica and the RabbitMQ pod in the middle of a booking and check that nothing is lost or
# done twice. Three requests for two slots each of a session that has four: exactly two are Confirmed, the third is told how many
# are left, every slots is held and no more, exactly two referral letters exist, and a cancelled order gives its slots back
# while its referral letter stays. Needs the cluster from `make cluster-up`.
set -euo pipefail

API="${API:-http://127.0.0.1:5238}"
CTX="${CTX:-kind-golang-clinic}"
K="kubectl --context $CTX -n clinic"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'
say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }
# every call retries a few times: for a moment after a pod dies the Service may still send a request to it
retry() { for _ in $(seq 1 20); do if out="$("$@" 2>/dev/null)"; then printf '%s' "$out"; return 0; fi; sleep 1; done; return 1; }

login() { retry curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"admin@clinic.local\",\"password\":\"${ADMIN_PASSWORD:-ClinicAdmin2026}\"}" | token_of; }
register() { retry curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$1-$RUN@example.com\",\"password\":\"Resil-Passw0rd1\"}" | token_of; }
ADM="Authorization: Bearer $(login)"
A="Authorization: Bearer $(register a)"
B="Authorization: Bearer $(register b)"
C="Authorization: Bearer $(register c)"

echo "1. a session of four slots, and two requests of two slots in flight"
retry curl -sf -X POST "$API/api/sessions" -H "$JSON" -H "$ADM" -d "{\"code\":\"RES-$RUN\",\"provider\":\"Resilience\",\"specialty\":\"One\",\"location\":\"Two\",\"capacitySlots\":4}" >/dev/null || fail "could not open the session"
order() { retry curl -sf -X POST "$API/api/appointments" -H "$JSON" -H "$1" -d "{\"sessionCode\":\"RES-$RUN\",\"reference\":\"REF-$RUN-$2\",\"slots\":2}" | jget "['id']"; }
IDA="$(order "$A" a)"; IDB="$(order "$B" b)"

echo "2. kill an API replica and the RabbitMQ pod while the requests are still in flight"
$K delete "$($K get pods -l app.kubernetes.io/name=api -o name | head -1)" --wait=false | sed 's/^/  /'
$K delete pod rabbitmq-0 --wait=false | sed 's/^/  /'
IDC="$(order "$C" c)" # accepted even now: the request and its outbox row are one local transaction
say "a third request was accepted while the broker was down"

echo "3. everything comes back: two are Confirmed, the third is told it is short, and nothing is oversold"
$K rollout status statefulset/rabbitmq --timeout=180s >/dev/null
$K rollout status deployment/api --timeout=180s >/dev/null
status() { body="$(retry curl -sf -H "$1" "$API/api/appointments/$2")" && printf '%s' "$body" | jget "['status']" || echo unknown; }
settled() { s="$(status "$1" "$2")"; [ "$s" = Confirmed ] || [ "$s" = Rejected ]; }
for _ in $(seq 1 240); do
  if settled "$A" "$IDA" && settled "$B" "$IDB" && settled "$C" "$IDC"; then break; fi
  sleep 1
done
SOLD=0; SHORT=0; WHO=""; WHOID=""
for pair in "$A|$IDA" "$B|$IDB" "$C|$IDC"; do
  s="$(status "${pair%%|*}" "${pair##*|}")"
  case "$s" in Confirmed) SOLD=$((SOLD + 1)); WHO="${pair%%|*}"; WHOID="${pair##*|}" ;; Rejected) SHORT=$((SHORT + 1)) ;; *) fail "a request is stuck in $s" ;; esac
done
[ "$SOLD" = 2 ] && [ "$SHORT" = 1 ] || fail "expected 2 Confirmed and 1 short, got $SOLD and $SHORT"
[ "$(retry curl -sf -H "$ADM" "$API/api/sessions/RES-$RUN" | jget "['remainingSlots']")" = 0 ] || fail "all four slots must be held, and no more"
DOCS=0
for id in "$IDA" "$IDB" "$IDC"; do
  if curl -sf -H "$ADM" "$API/api/letters/$id" >/dev/null 2>&1; then DOCS=$((DOCS + 1)); fi
done
[ "$DOCS" = 2 ] || fail "exactly two referral letters may exist, found $DOCS"
say "2 Confirmed, 1 short, 0 slots left, 2 referral letters"

echo "4. a cancelled request gives its slots back and its referral letter stays"
retry curl -sf -X POST -H "$WHO" "$API/api/appointments/$WHOID/cancellation" >/dev/null || fail "the cancellation failed"
for _ in $(seq 1 60); do [ "$(retry curl -sf -H "$ADM" "$API/api/sessions/RES-$RUN" | jget "['remainingSlots']")" = 2 ] && break; sleep 1; done
[ "$(retry curl -sf -H "$ADM" "$API/api/sessions/RES-$RUN" | jget "['remainingSlots']")" = 2 ] || fail "two slots must be back"
curl -sf -H "$ADM" "$API/api/letters/$WHOID" >/dev/null || fail "the referral letter must stay"
say "2 slots back, the referral letter stays"
echo "resilience passed"
