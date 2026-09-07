#!/bin/sh
# Publish lasso's usage limits into the Luvus Bar. lasso computes the segments
# (`lasso usage-bar`) because it already holds the provider credentials, token
# refresh, rate-limit backoff and last-good cache; Luvus owns the rendering.
set -eu

luvus="${LUVUS_BIN_PATH:-luvus}"
lasso="${LUVUS_SETTING_LASSO_BIN:-}"
[ -n "$lasso" ] || lasso="$(command -v lasso || true)"
if [ -z "$lasso" ]; then
  "$luvus" bar push --id usage --content \
    '[{"type":"text","text":"usage: lasso not found","tone":"warning"}]'
  exit 1
fi

compact_flag=""
if [ "${LUVUS_SETTING_COMPACT:-false}" = "true" ]; then
  compact_flag="-compact"
fi

if ! content="$("$lasso" usage-bar $compact_flag)"; then
  "$luvus" bar push --id usage --content \
    '[{"type":"text","text":"usage: lasso 3.0+ required","tone":"warning"}]'
  exit 1
fi
"$luvus" bar push --id usage --content "$content"
