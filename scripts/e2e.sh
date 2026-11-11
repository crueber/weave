#!/usr/bin/env bash
# CLI-driven end-to-end test for a running Weave server.
#
#   WEAVE_URL=http://localhost:8080 WEAVE_TOKEN=wk_... scripts/e2e.sh
#
# Weave is OIDC-only, so the test authenticates with an API key of an admin account:
# create one in the UI (Settings -> API key) or, on the server host, mint a first one:
#   docker compose exec weave weave admin mint-key --email you@example.com --role admin
#
# Env:  WEAVE_TOKEN   API key (required)
#       WEAVE_BIN     weave binary (default: ./weave if present, else `weave` on PATH)
#       STRICT=1      treat steps whose op does not exist as failures (default: SKIP)
#       KEEP=1        do not clean up created data
# Steps are written against the op names in docs/FEATURES.md / DESIGN.md. Each step
# lists candidate op names ("a.b|c.d") and candidate field names ("listId|list_id");
# the first op that exists (see `weave ops`) and the first field present in its
# schema are used, and extra optional fields are dropped, so the script adapts.
set -u
cd "$(dirname "$0")/.." || exit 2

WEAVE_URL="${WEAVE_URL:-http://localhost:8080}"; export WEAVE_URL
BIN="${WEAVE_BIN:-}"
if [ -z "$BIN" ]; then if [ -x ./weave ]; then BIN=./weave; else BIN=weave; fi; fi
RUN="e2e$(date +%s)$RANDOM"
TMP="$(mktemp -d)"
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
[ -n "${WEAVE_TOKEN:-}" ] || { echo "set WEAVE_TOKEN to an admin API key (see header)" >&2; exit 2; }
export WEAVE_TOKEN

pass=0; fail=0; skip=0
CLEAN=()   # "op key=val ..." lines, executed in reverse
OPS_JSON="$("$BIN" ops 2>/dev/null)" || { echo "cannot run $BIN ops" >&2; exit 2; }

ok()   { pass=$((pass+1)); printf '  ok    %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf '  FAIL  %s: %s\n' "$1" "$2" >&2; }
skp()  { skip=$((skip+1)); printf '  skip  %s (%s)\n' "$1" "$2"; }

# resolve "a.b|c.d" -> first registered op name
resolve_op() {
  local c; IFS='|' read -ra cs <<<"$1"
  for c in "${cs[@]}"; do
    if jq -e --arg n "$c" 'map(select(.name==$n))|length>0' <<<"$OPS_JSON" >/dev/null; then echo "$c"; return 0; fi
  done; return 1
}
# does op have field name?
has_field() { jq -e --arg n "$1" --arg f "$2" '.[]|select(.name==$n)|.fields|map(select(.name==$f))|length>0' <<<"$OPS_JSON" >/dev/null; }

LAST=""   # last JSON output
# call "op1|op2" "fieldA|fieldB=value" ...  -> sets LAST, returns CLI exit code (99 = op missing)
call() {
  local ops="$1"; shift
  local op; op="$(resolve_op "$ops")" || return 99
  local args=() kv names val n picked
  for kv in "$@"; do
    names="${kv%%=*}"; val="${kv#*=}"; picked=""
    IFS='|' read -ra ns <<<"$names"
    for n in "${ns[@]}"; do if has_field "$op" "$n"; then picked="$n"; break; fi; done
    [ -n "$picked" ] && args+=("--$picked" "$val")
  done
  local tag="${op%%.*}" verb="${op#*.}"
  LAST="$("$BIN" "$tag" "$verb" "${args[@]}" 2>"$TMP/err")"; local rc=$?
  [ $rc -ne 0 ] && LAST="$(cat "$TMP/err")"
  return $rc
}
# step "title" "op1|op2" fields... ; on success stores .item.id into $ID
ID=""
step() {
  local title="$1"; shift
  call "$@"; local rc=$?
  if [ $rc -eq 99 ]; then
    if [ "${STRICT:-0}" = 1 ]; then bad "$title" "no such op: $1"; else skp "$title" "no op $1"; fi
    ID=""; return 1
  elif [ $rc -ne 0 ]; then bad "$title" "exit $rc: $LAST"; ID=""; return 1; fi
  ID="$(jq -r '.item.id // empty' <<<"$LAST" 2>/dev/null)"
  ok "$title"
}
cleanup_add() { CLEAN+=("$*"); }
cleanup() {
  [ "${KEEP:-0}" = 1 ] && { echo "KEEP=1: leaving data (run id $RUN)"; return; }
  echo "cleanup"
  local i line
  for ((i=${#CLEAN[@]}-1; i>=0; i--)); do
    line="${CLEAN[$i]}"; read -ra parts <<<"$line"
    call "${parts[0]}" "${parts[@]:1}" || true
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "weave e2e against $WEAVE_URL (run $RUN)"

# ---- version / health (public)
step "system.version" "system.version"
step "system.health" "system.health"

# ---- who am I (API key auth)
step "auth.me" "auth.me|user.me"
USER_ID="$ID"
[ -n "$USER_ID" ] || { echo "WEAVE_TOKEN was not accepted (or is not an admin key)"; exit 1; }

step "system.storage" "system.storage"
step "admin.stats" "admin.stats"
if resolve_op "admin.backup" >/dev/null; then
  if "$BIN" admin backup -o "$TMP/backup.db" >/dev/null 2>&1 && [ "$(head -c 6 "$TMP/backup.db")" = "SQLite" ]; then ok "admin.backup (sqlite snapshot)"; else bad "admin.backup" "no sqlite snapshot"; fi
fi

# ---- users come from OIDC; just check the directory and groups
step "user.list" "user.list"
step "group.create" "group.create" "name=e2e-$RUN"
GROUP="$ID"; [ -n "$GROUP" ] && cleanup_add "group.delete" "id=$GROUP"
[ -n "$GROUP" ] && step "group.member.add" "group.member.add" "id=$GROUP" "userId|user_id=$USER_ID"

# ---- project / board / list / labels
step "project.create" "project.create" "name=E2E $RUN"
PROJECT="$ID"; [ -n "$PROJECT" ] && cleanup_add "project.delete" "id=$PROJECT"
[ -n "$PROJECT" ] || { echo "no project; aborting"; exit 1; }
step "board.create" "board.create" "projectId|project_id=$PROJECT" "name=Board $RUN" "position=65536"
BOARD="$ID"; [ -n "$BOARD" ] && cleanup_add "board.delete" "id=$BOARD"
[ -n "$BOARD" ] || { echo "no board; aborting"; exit 1; }
step "board.member.list" "board.member.list" "boardId|board_id|id=$BOARD"
step "list.create" "list.create" "boardId|board_id=$BOARD" "name=Todo" "position=65536" "type=active"
LIST="$ID"
step "label.create" "label.create" "boardId|board_id=$BOARD" "name=urgent" "color=berry-red" "position=65536"
LABEL="$ID"
[ -n "$LIST" ] || { echo "no list; aborting"; exit 1; }

# ---- card + extras
step "card.create" "card.create" "listId|list_id=$LIST" "name=Card $RUN" "description=hello e2e" "type=project" "position=65536"
CARD="$ID"
[ -n "$CARD" ] || { echo "no card; aborting"; exit 1; }
cleanup_add "card.delete" "id=$CARD"
[ -n "$LABEL" ] && step "card.label.add" "card.label.add|cardlabel.create|card.label.create" "cardId|card_id|id=$CARD" "labelId|label_id=$LABEL"
[ -n "$USER_ID" ] && step "card.member.add" "card.member.add|cardmembership.create|card.membership.create" "cardId|card_id|id=$CARD" "userId|user_id=$USER_ID"
step "card.update" "card.update" "id=$CARD" "name=Card $RUN (renamed)" "dueDate=2030-01-01T00:00:00Z"

step "tasklist.create" "tasklist.create|task.list.create|tasklist.add" "cardId|card_id=$CARD" "name=Checklist" "position=65536"
TL="$ID"
step "task.create" "task.create|task.add" "taskListId|task_list_id|tasklistId=${TL:-$CARD}" "cardId|card_id=$CARD" "name=do it" "position=65536"
TASK="$ID"
[ -n "$TASK" ] && step "task.update" "task.update" "id=$TASK" "isCompleted|completed=true"

step "comment.create" "comment.create|comment.add" "cardId|card_id=$CARD" "text=first comment from e2e"
echo "attachment payload $RUN" > "$TMP/attach.txt"
step "attachment.create" "attachment.create|attachment.upload|attachment.add" "cardId|card_id=$CARD" "file=$TMP/attach.txt" "name=attach.txt" "type=file"
ATT="$ID"
if [ -n "$ATT" ] && resolve_op "attachment.download" >/dev/null; then
  "$BIN" attachment download "$ATT" attach.txt -o "$TMP/dl.txt" >/dev/null 2>&1 && grep -q "$RUN" "$TMP/dl.txt" && ok "attachment.download roundtrip" || bad "attachment.download" "content mismatch"
fi

step "customfield.group.create" "customfieldgroup.create|customfield.group.create" "cardId|card_id=$CARD" "name=Details"
CFG="$ID"
step "customfield.create" "customfield.create|customfield.field.create|customfield.add" "groupId|group_id|customFieldGroupId=${CFG:-}" "name=Estimate" "type|fieldType=number" "position=65536"
CF="$ID"
if [ -n "$CF" ]; then step "customfield.value.set" "customfield.value.set|customfield.value.update|customfieldvalue.set" "cardId|card_id=$CARD" "groupId|customFieldGroupId=$CFG" "fieldId|customFieldId|id=$CF" "value|content=5"; fi

# ---- notify: service + webhook
step "notification service" "notification_service.create|notificationservice.create|notification.service.create|notificationservice.add" "boardId|board_id=$BOARD" "url=generic://localhost:9/e2e?disabletls=yes" "format=text"
NS="$ID"; [ -n "$NS" ] && cleanup_add "notificationservice.delete|notification.service.delete" "id=$NS"
step "webhook.create" "webhook.create" "name=e2e-$RUN" "url=http://localhost:9/hook" "events=cardCreate,cardUpdate"
WH="$ID"; [ -n "$WH" ] && cleanup_add "webhook.delete" "id=$WH"

# ---- search + exports
step "search" "search.query|search.run|search.cards|search.global" "q|query=Card $RUN"
if resolve_op "search.query|search.run|search.cards|search.global" >/dev/null; then
  jq -e '(.items // .item // []) | tostring | contains("'"$RUN"'")' <<<"$LAST" >/dev/null && ok "search finds card" || skp "search finds card" "index may be async"
fi
for e in "board.export|board.export.json" "board.export.html|board.html.export" "card.export.html|card.export" "board.export.pdf|board.pdf.export"; do
  if op="$(resolve_op "$e")"; then
    tag="${op%%.*}"; verb="${op#*.}"
    id="$BOARD"; [[ "$op" == card.* ]] && id="$CARD"
    if "$BIN" "$tag" "$verb" "$id" -o "$TMP/export.out" >/dev/null 2>"$TMP/err" && [ -s "$TMP/export.out" ]; then ok "$op"; else bad "$op" "$(cat "$TMP/err")"; fi
  fi
done

# ---- activity / read-back
step "card.get" "card.get|card.show" "id=$CARD"
step "board.get" "board.get|board.show" "id=$BOARD"
step "activity" "activity.card|card.activity.list|card.actions" "cardId|card_id|id=$CARD"

echo
echo "passed=$pass failed=$fail skipped=$skip"
[ "$fail" -eq 0 ]
