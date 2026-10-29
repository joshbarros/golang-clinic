#!/usr/bin/env bash
# Drives the stack running in Kubernetes over HTTP. Every code carries a run id, so a kept Postgres volume never collides.
# Needs: curl, python3. The stack must be up: make cluster-up
set -euo pipefail

API="${API:-http://127.0.0.1:5238}"
RABBIT="${RABBIT:-http://127.0.0.1:15728}"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'

say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }

ADMIN_EMAIL="${ADMIN_EMAIL:-admin@clinic.local}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-ClinicAdmin2026}"
CUSTOMER_PASSWORD="Smoke-Passw0rd1"
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }

open_session() { # code capacity -> prints nothing
  curl -sf -X POST "$API/api/sessions" -H "$JSON" -H "$ADM" \
    -d "{\"code\":\"$1\",\"provider\":\"Dr. Smoke\",\"specialty\":\"Cardiology\",\"location\":\"Clinic A\",\"capacitySlots\":$2}" >/dev/null \
    || fail "could not open session $1"
}
book() { # code reference slots -> prints appointment id
  curl -sf -X POST "$API/api/appointments" -H "$JSON" -H "$CUS" \
    -d "{\"sessionCode\":\"$1\",\"reference\":\"$2\",\"slots\":$3}" | jget "['id']"
}
appointment_field() { curl -sf -H "$ADM" "$API/api/appointments/$1" | jget "['$2']"; }
remaining() { curl -sf -H "$ADM" "$API/api/sessions/$1" | jget "['remainingSlots']"; }

wait_for() { # description command... : polls up to 30s until the command succeeds
  local what="$1"; shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  fail "timed out waiting for $what"
}
status_is() { [ "$(appointment_field "$1" status)" = "$2" ]; }
remaining_is() { [ "$(remaining "$1")" = "$2" ]; }
terminal() { case "$(appointment_field "$1" status)" in Confirmed|Rejected|Cancelled) return 0 ;; *) return 1 ;; esac; }
event_types() { curl -sf -H "$ADM" "$API/api/appointments/$1/events" | python3 -c "import sys,json; print(' '.join(e['type'] for e in json.load(sys.stdin)))"; }
dlq_is() { [ "$(dlq_count)" = "$1" ]; }
dlq_count() { curl -sf -u platform:platform "$RABBIT/api/queues/%2F/referrals.work.dlq" | jget "['messages']"; }

curl -sf "$API/api/health" >/dev/null || fail "API is not healthy at $API; run: make cluster-up"

echo "0. authentication and ownership"
[ "$(code_of "$API/api/sessions")" = 401 ] || fail "an anonymous request should be 401"
[ "$(code_of -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"wrong-Passw0rd\"}")" = 401 ] || fail "a wrong password should be 401"
ADM="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | token_of)" || fail "the administrator could not sign in"
CUS="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"smoke-$RUN@example.com\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)" || fail "could not register a customer"
OTHER="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"other-$RUN@example.com\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)"
[ "$(code_of -X POST "$API/api/sessions" -H "$JSON" -H "$CUS" -d '{"code":"X"}')" = 403 ] || fail "a customer must not open sessions"
open_session "A-$RUN" 5
IDA="$(book "A-$RUN" "PT-$RUN-OWN" 1)"
[ "$(code_of -H "$OTHER" "$API/api/appointments/$IDA")" = 404 ] || fail "another customer must get 404 for someone else's appointment"
[ "$(code_of -H "$CUS" "$API/api/appointments/$IDA/events")" = 403 ] || fail "the event log is staff only"
[ "$(code_of -X POST -H "$OTHER" "$API/api/appointments/$IDA/cancellation")" = 404 ] || fail "another customer must not cancel it"
say "401 anonymous, 401 bad password, 403 customer opening a session, 404 for another customer, 403 on the event log"

echo "0b. password reset ends the old sessions (needs the dev mailbox the demo stack turns on)"
RESET_EMAIL="reset-$RUN@example.com"
OLD="$(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)"
[ "$(code_of -H "Authorization: Bearer $OLD" "$API/api/auth/me")" = 200 ] || fail "the new session should work"
[ "$(code_of -X POST "$API/api/auth/forgot-password" -H "$JSON" -d "{\"email\":\"nobody-$RUN@example.com\"}")" = 202 ] || fail "an unknown address must look the same"
curl -sf -X POST "$API/api/auth/forgot-password" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\"}" >/dev/null || fail "forgot-password failed"
LINK="$(curl -sf "$API/api/dev/mailbox?to=$RESET_EMAIL" | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['link'])")"
link_param() { python3 -c "import sys,urllib.parse as u; print(u.parse_qs(u.urlparse(sys.argv[1]).query)['$1'][0])" "$LINK"; }
[ "$(code_of -X POST "$API/api/auth/reset-password" -H "$JSON" -d "{\"userId\":\"$(link_param reset)\",\"token\":\"$(link_param token)\",\"newPassword\":\"Another-Passw0rd9\"}")" = 204 ] || fail "reset failed"
sleep 3 # the session version is cached for a moment
[ "$(code_of -H "Authorization: Bearer $OLD" "$API/api/auth/me")" = 401 ] || fail "the old access token should be revoked"
[ "$(code_of -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\",\"password\":\"Another-Passw0rd9\"}")" = 200 ] || fail "the new password should work"
say "reset link works once, the old access token is revoked, the new password signs in"

echo "1. input is validated at the door"
open_session "V-$RUN" 10
for body in \
  "{\"sessionCode\":\"V-$RUN\",\"reference\":\"\",\"slots\":1}" \
  "{\"sessionCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"slots\":0}" \
  "{\"sessionCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"slots\":5}" \
  "{\"sessionCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"slots\":4294967297}" \
  "{\"sessionCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"slots\":9223372036854775807}"; do
  code="$(code_of -X POST "$API/api/appointments" -H "$JSON" -H "$CUS" -d "$body")"
  [ "$code" = 400 ] || fail "expected 400 for $body, got $code"
done
say "empty, zero, over-limit and overflowing slot counts are all 400"

echo "2. happy path"
open_session "H-$RUN" 10
ID="$(book "H-$RUN" "PT-$RUN-1" 3)"
wait_for "appointment to be Confirmed" status_is "$ID" Confirmed
[ "$(appointment_field "$ID" feeCents)" = 37500 ] || fail "fee should be 3 x 12000 + 1500 cents"
[ "$(curl -sf -H "$ADM" "$API/api/letters/$ID" | jget "['status']")" = Issued ] || fail "referral letter not issued"
wait_for "remaining slots to be 7" remaining_is "H-$RUN" 7
TYPES="$(event_types "$ID")"
for t in slots.reserve slots.held letter.issued; do
  case "$TYPES" in *"$t"*) ;; *) fail "Kafka log is missing $t (has: $TYPES)" ;; esac
done
say "confirmed, fee 37500, remaining 7, Kafka log has the three steps"

echo "3. last unit: three appointments, one commits, two are short and write no hold"
open_session "L-$RUN" 1
TMP="$(mktemp -d)"
for n in A B C; do ( book "L-$RUN" "PT-$RUN-$n" 1 >"$TMP/$n" ) & done
wait
IDS=""
for n in A B C; do [ -s "$TMP/$n" ] || fail "appointment $n was not accepted"; IDS="$IDS $(cat "$TMP/$n")"; done
rm -rf "$TMP"
for i in $IDS; do wait_for "appointment $i to settle" terminal "$i"; done
CONFIRMED=0; SHORT=0
for i in $IDS; do
  s="$(appointment_field "$i" status)"
  if [ "$s" = Confirmed ]; then CONFIRMED=$((CONFIRMED + 1)); fi
  if [ "$s" = Rejected ]; then
    case "$(appointment_field "$i" reason)" in Short*) SHORT=$((SHORT + 1)); case "$(event_types "$i")" in *slots.held*) fail "the short loser has a hold" ;; esac ;; esac
  fi
done
[ "$CONFIRMED" = 1 ] && [ "$SHORT" = 2 ] || fail "expected 1 confirmed and 2 short, got $CONFIRMED confirmed and $SHORT short"
[ "$(remaining "L-$RUN")" = 0 ] || fail "remaining should be 0"
say "1 confirmed, 2 short, remaining 0, no hold for the losers"

echo "4. poison: committed as rejected, dead-lettered, the slot comes back"
open_session "P-$RUN" 5
BEFORE="$(dlq_count)"
IDP="$(book "P-$RUN" "PT-POISON-$RUN" 2)"
wait_for "poison appointment to be Rejected" status_is "$IDP" Rejected
wait_for "slots to be restored to 5" remaining_is "P-$RUN" 5
wait_for "referrals.work.dlq to gain one message" dlq_is $((BEFORE + 1))
[ "$(curl -sf -H "$ADM" "$API/api/letters/$IDP" | jget "['status']")" = Rejected ] || fail "rejection should be committed on the letter row"
[ "$(appointment_field "$IDP" slotsReleasedAt)" != None ] || fail "appointment should record the release"
say "rejected, dead-lettered (+1 in referrals.work.dlq), remaining back to 5"

echo "4b. dead letters can be listed by staff only, and replayed"
[ "$(code_of -H "$CUS" "$API/api/admin/dead-letters")" = 403 ] || fail "dead letters are staff only"
curl -sf -H "$ADM" "$API/api/admin/dead-letters" | python3 -c "import sys,json; q={x['queue']:x for x in json.load(sys.stdin)}; assert q['referrals.work']['count']>=1, q" || fail "the dead-letter queue should list the poison message"
BEFORE_REPLAY="$(dlq_count)"
REPLAYED="$(curl -sf -X POST -H "$ADM" "$API/api/admin/dead-letters/referrals.work/replay?max=100" | jget "['replayed']")" || fail "replay failed"
[ "$REPLAYED" -ge 1 ] || fail "replay should have taken at least one message"
# The poison reference still fails for the same reason, so it is retried and dead-lettered again: replay is for fixed causes.
wait_for "the replayed poison message to be dead-lettered again" dlq_is "$BEFORE_REPLAY"
say "listed, replayed $REPLAYED, dead-lettered again because the cause is still there"

echo "5. cancellation races the final mark: the cancellation wins, the issued letter stays"
open_session "R-$RUN" 6
WON=0
for attempt in 1 2 3; do
  IDR="$(book "R-$RUN" "PT-$RUN-R$attempt" 2)"
  # The referral letter exists before the appointment is marked Confirmed (the final mark is delayed in the demo cluster).
  wait_for "referral letter to exist" curl -sf -H "$ADM" "$API/api/letters/$IDR"
  if status_is "$IDR" Held; then WON=1; break; fi
  say "window missed on attempt $attempt (appointment already Confirmed); the cancellation still has to hold; retrying"
  curl -sf -X POST -H "$CUS" "$API/api/appointments/$IDR/cancellation" >/dev/null || true
  wait_for "appointment to be Cancelled" status_is "$IDR" Cancelled
done
[ "$WON" = 1 ] || fail "never caught the appointment between letter issued and final mark"
curl -sf -X POST -H "$CUS" "$API/api/appointments/$IDR/cancellation" >/dev/null || fail "cancellation was refused"
wait_for "appointment to be Cancelled" status_is "$IDR" Cancelled
sleep 3 # longer than the final-mark delay: the late mark must not undo the cancellation
status_is "$IDR" Cancelled || fail "the final mark overwrote the cancellation"
[ "$(curl -sf -H "$ADM" "$API/api/letters/$IDR" | jget "['status']")" = Issued ] || fail "the issued letter was rolled back"
[ "$(appointment_field "$IDR" letterNumber)" = None ] || fail "the late final mark should have been skipped"
wait_for "slots to come back" remaining_is "R-$RUN" 6
code="$(code_of -X POST -H "$CUS" "$API/api/appointments/$IDR/cancellation")"
[ "$code" = 409 ] || fail "second cancellation should be 409, got $code"
say "appointment Cancelled, letter still Issued, remaining 6, second cancellation 409"

echo "golang-clinic smoke passed"
