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

# Settings → CLI flags. The order enum uses short keys; the visible-provider
# list hands lasso the provider names it reports, in that order.
providers=""
for key in $(printf '%s' "${LUVUS_SETTING_ORDER:-claude,kimi,codex,zai}" | tr ',' ' '); do
  case "$key" in
    claude) shown="${LUVUS_SETTING_SHOW_CLAUDE:-true}"; name="Claude Code" ;;
    kimi)   shown="${LUVUS_SETTING_SHOW_KIMI:-true}";   name="Kimi Code" ;;
    codex)  shown="${LUVUS_SETTING_SHOW_CODEX:-true}";  name="Codex" ;;
    zai)    shown="${LUVUS_SETTING_SHOW_ZAI:-true}";    name="Z.ai" ;;
    *) continue ;;
  esac
  if [ "$shown" = "true" ]; then providers="${providers:+$providers,}$name"; fi
done

set -- -providers "$providers"
if [ "${LUVUS_SETTING_COMPACT:-false}" = "true" ]; then set -- "$@" -compact; fi

if ! out="$("$lasso" usage-bar "$@")"; then
  "$luvus" bar push --id usage --content \
    '[{"type":"text","text":"usage: lasso 3.0+ required","tone":"warning"}]'
  exit 1
fi
# Split the two arrays without jq: lasso emits exactly
# {"content":[…],"compact_content":[…]}.
content="${out#*\"content\":}"; content="${content%%,\"compact_content\":*}"
compact="${out#*\"compact_content\":}"; compact="${compact%\}}"
"$luvus" bar push --id usage --content "$content" --compact-content "$compact"
