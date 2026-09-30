#!/usr/bin/env bash
# QUICKSTART.md, run as written, in a throwaway copy of the quickstart
# compose project: the local chain, two hosts and MySQL; three homes (a
# sender, a recipient, host-a's payee); an office; a message sent in
# unicast mode, listed at both hosts, read and acknowledged; a payment
# inside an envelope internalized; the priced history question paid; the
# payee settling it; a message dropped.
#
#   scripts/quickstart-check.sh
#
# Needs the three images the compose file names (make docker-build, make
# docker-build-host, or the published ones). COMPOSE names the compose
# command (default "docker compose", else Compose from the docker:cli
# image with the work directory mounted where it is). Everything it
# creates, volumes included, is removed at the end, pass or fail.
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project=bbox-quickstart-check-$$
cp "$here/deploy/quickstart/compose.yaml" "$work/"
cd "$work"
if [ -n "${COMPOSE:-}" ]; then
	compose=$COMPOSE
elif docker compose version >/dev/null 2>&1; then
	compose="docker compose"
else
	compose="docker run --rm -i -v /var/run/docker.sock:/var/run/docker.sock -v $work:$work -w $work docker:cli compose"
fi

dc() { $compose -p "$project" "$@"; }
as() { local who=$1; shift; dc run --rm -T -e "BBOX_HOME=/home/nonroot/.bbox/$who" bbox "$@"; }
cleanup() {
	dc --profile cli down -v --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
step() { printf '\n== %s\n' "$*"; }
fail() {
	echo "quickstart-check: $*" >&2
	dc logs --no-color --tail 40 >&2 || true
	exit 1
}

step "1. the local chain"
dc up -d chain

step "2. three homes, an office, the payee key"
as alice init
as alice fund
as bob init | tee bob.txt
as bob fund
BOB=$(sed -n 's/^identity *//p' bob.txt)
[ -n "$BOB" ] || fail "init printed no identity"
as payee init
as bob office new post | tee office.txt
sed -n 's/^host *//p' office.txt >.env
as payee payee key -out - >>.env
grep -q '^BBOX_OFFICES=post_[a-z]\{10\}$' .env && grep -q '^BBOX_PAYEE_KEY=[0-9a-f]\{64\}$' .env || fail "the .env lines are missing"

step "3. two hosts that carry the office"
dc up -d
for i in $(seq 1 90); do
	out=$(as bob doctor 2>/dev/null || true)
	[ "$(printf '%s\n' "$out" | grep -c '^host .* answering')" = 2 ] && printf '%s\n' "$out" | grep -q '^history .* serves terms' && break
	sleep 2
done
printf '%s\n' "$out"
[ "$(printf '%s\n' "$out" | grep -c '^host .* answering')" = 2 ] || fail "the hosts did not come up"

step "4. send, list at both hosts, read, acknowledge"
as alice send "$BOB" -m 'hello bob, from the quickstart' | tee sent.txt
TX=$(sed -n 's/^sent *//p' sent.txt)
grep -q '^host http://host-a:8080: took 1 object(s), missed 0' sent.txt || fail "host-a did not take the envelope"
grep -q '^host http://host-b:8080: took 1 object(s), missed 0' sent.txt || fail "host-b did not take the envelope"
as bob list | tee list.txt
grep -q "$TX .*\[http://host-a:8080, http://host-b:8080\]" list.txt || fail "the hosts do not agree on the box"
as bob read "$TX" | tee read.txt
grep -q '^hello bob, from the quickstart$' read.txt || fail "read did not print the message"
as bob ack "$TX" | grep 'acknowledged 1 envelope(s)' || fail "ack"
as bob list | grep -q "$TX" && fail "the envelope is still open after the receipt"

step "5. a payment inside an envelope, internalized"
as alice send "$BOB" -m 'for the coffee' -pay 5000 | tee paid.txt
PTX=$(sed -n 's/^sent *//p' paid.txt)
as bob internalize "$PTX" | tee internalize.txt
grep -q '^internalized 5000 sat from ' internalize.txt || fail "internalize"

step "6. the priced question, and the payee settling it"
as bob terms | tee terms.txt
grep -q '^history        5 sat a question' terms.txt || fail "terms"
as bob history | tee history.txt
grep -q '^paid 5 sat to ' history.txt && grep -q "$TX" history.txt || fail "history"
as payee payee settle /var/lib/bbox-a/payments.jsonl | tee settle.txt
grep -q '^1 payment(s) settled, 5 sat' settle.txt || fail "payee settle"

step "7. drop"
as alice send "$BOB" -m 'take this back' | tee gone.txt
GONE=$(sed -n 's/^sent *//p' gone.txt)
as alice drop "$GONE" | grep 'retracted 1 funding output(s)' || fail "drop"
as bob list | grep -q "$GONE" && fail "the dropped envelope is still answered"

echo
echo "quickstart-check: ok"
