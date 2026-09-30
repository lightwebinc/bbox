#!/bin/sh
# Starts the reference overlay host with the bbox module. Two things are
# added to the stock start:
#
#  - BBOX_OFFICES is required, and when OVERLAY_TOPICS is unset it is
#    derived from it (tm_bbox_<office> for each entry), so a host is
#    configured by one variable: the line `bbox office new` prints. The
#    module checks the grammar.
#  - an unset OVERLAY_ADMIN_TOKEN is replaced by a random one for this run,
#    because the host requires one. Nothing can use the admin surface until
#    the operator sets their own.
set -eu

if [ -z "${BBOX_OFFICES:-}" ]; then
  echo "bbox-host: BBOX_OFFICES is empty. Create an office with 'bbox office new <name>' and set BBOX_OFFICES to the office it prints (several offices: comma separated)." >&2
  exit 64
fi

if [ -z "${OVERLAY_TOPICS:-}" ]; then
  topics=""
  for office in $(printf '%s' "$BBOX_OFFICES" | tr ',' ' '); do
    topics="${topics:+$topics,}tm_bbox_$office"
  done
  OVERLAY_TOPICS=$topics
  export OVERLAY_TOPICS
fi

if [ -z "${OVERLAY_ADMIN_TOKEN:-}" ]; then
  OVERLAY_ADMIN_TOKEN=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
  export OVERLAY_ADMIN_TOKEN
  echo "bbox-host: OVERLAY_ADMIN_TOKEN is unset; using a random one for this run (set it to use /admin)" >&2
fi

echo "bbox-host: topics $OVERLAY_TOPICS" >&2
exec node /app/dist/index.js "$@"
