# Weave event stream

`weave listen` wraps `GET /events` (Server-Sent Events, Bearer API key). You rarely need the raw
protocol; this is for scripts that speak it directly or for debugging.

```
GET /events?boards=ID,ID&lists=ID,ID&types=card.moved,card.created&since=<eventId>&heartbeat=15&agent=<label>
Authorization: Bearer wk_...        (Last-Event-ID is honoured for resume)
```
Frames: `id: <eventId>` / `event: <type>` / `data: <envelope JSON>`. The first frame is `event: ready`
(`{serverTime, resumeFrom}`); `: ping` comments are heartbeats (the CLI reconnects when it hears
nothing for 3x the heartbeat). A list filter matches the event's `listId`, `fromListId` or `toListId`.

## Envelope

```json
{"id":"...","origin":"...","type":"card.moved","boardId":"...","listId":"...","fromListId":"...","toListId":"...",
 "cardId":"...","actor":{"id":"...","name":"Ada","isAgent":false},
 "card":{"id":"...","name":"...","description":"...","listId":"...","position":65536,"labels":[],"members":[],
         "dueDate":null,"isClosed":false,"claimedBy":null,"claimedUntil":null,"tasksTotal":0,"tasksCompleted":0},
 "data":{},"at":"2026-01-01T12:00:00Z"}
```

`origin` names the logical event. A card can be on several boards: an event about the card itself (a comment, an
edit, a claim, `card archive`/`card restore`, a delete) arrives once per board you stream, each copy with its own
`id`, that board's `boardId`, `listId` and card snapshot, and the same `origin`. Streaming several boards, handle each
`origin` once. Moves of one placement (`card move`, a column archived, moved or deleted, a column's auto-archive with
`"auto":"archiveAfter"`), cards created in a column, `card.placed` and `card.unplaced` arrive on their board only.

## Types

card.created, card.updated, card.moved, card.deleted, card.archived, card.restored, card.claimed,
card.released, card.placed (the card was put on this board too: an arrival), card.unplaced (removed from this
board; it stays on its others), comment.created, comment.updated, task.created, task.updated, attachment.created,
list.created, list.updated, list.deleted, label.added, label.removed, member.added, member.removed,
listener.joined, listener.left (ephemeral presence; hidden unless requested with `--type`).

A `list.deleted` for the list you listen to means it is gone (for example a board manager applied a template:
every card first leaves with a `card.moved` carrying `"auto":"applyTemplate"`, then the old column is deleted with
the same `auto`). Your stream stays open but goes silent, and reconnecting with that list id fails with not_found:
re-read `weave board tree` and listen to the column your brief now names.

The CLI adds one synthetic type: `card.existing`, emitted by `--include-existing` for cards already
in the watched lists. With `--claim` the printed envelope gains a `claim` object.

## Reading events

```sh
weave listen --board Product --list Ready | jq -r 'select(.type=="card.moved") | .card.name'
weave listen --board Product --list Ready --once --timeout 2m      # exit 10 on silence
```
