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
{"id":"...","type":"card.moved","boardId":"...","listId":"...","fromListId":"...","toListId":"...",
 "cardId":"...","actor":{"id":"...","name":"Ada","isAgent":false},
 "card":{"id":"...","name":"...","description":"...","listId":"...","position":65536,"labels":[],"members":[],
         "dueDate":null,"isClosed":false,"claimedBy":null,"claimedUntil":null},
 "data":{},"at":"2026-01-01T12:00:00Z"}
```

## Types

card.created, card.updated, card.moved, card.deleted, card.archived, card.restored, card.claimed,
card.released, comment.created, comment.updated, task.created, task.updated, attachment.created,
list.created, list.updated, list.deleted, label.added, label.removed, member.added, member.removed,
listener.joined, listener.left (ephemeral presence; hidden unless requested with `--type`).

The CLI adds one synthetic type: `card.existing`, emitted by `--include-existing` for cards already
in the watched lists. With `--claim` the printed envelope gains a `claim` object.

## Reading events

```sh
weave listen --board Product --list Ready | jq -r 'select(.type=="card.moved") | .card.name'
weave listen --board Product --list Ready --once --timeout 2m      # exit 10 on silence
```
