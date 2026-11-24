# Webhooks: delivery and signature verification

Weave POSTs a JSON body to each matching webhook (`webhook.*` ops, `/admin/webhooks`, Board settings > Webhooks).
Every delivery carries:

| Header | Value |
|---|---|
| `X-Weave-Event` | event name, e.g. `cardCreate` |
| `X-Weave-Delivery` | delivery id (stable across retries; use it to de-duplicate) |
| `X-Weave-Timestamp` | unix seconds when this attempt was sent |
| `X-Weave-Signature-V2` | `t=<unix ts>,v1=<hex HMAC-SHA256(secret, ts + "." + body)>` (recommended) |
| `X-Weave-Signature` | `sha256=<hex HMAC-SHA256(secret, body)>` (legacy, unchanged) |
| `Authorization` | `Bearer <access token>` when the webhook has one |

The signing key is the webhook's `secret` (or its access token when no secret is set).
Both signature headers are sent only when a key exists.

## Why V2

The legacy signature covers only the body, so a captured request can be replayed forever.
`X-Weave-Signature-V2` puts the timestamp inside the MAC, so a receiver that checks the
timestamp window rejects replays. Existing receivers that verify `X-Weave-Signature` keep
working; migrate to V2 when convenient.

## Verifying (receiver side)

1. Read the **raw** request body (do not re-serialise parsed JSON).
2. Parse `t` and `v1` from `X-Weave-Signature-V2`.
3. Reject if `|now - t| > 300` seconds (5 minute replay window; keep clocks on NTP).
4. Compute `HMAC-SHA256(secret, t + "." + rawBody)` as hex.
5. Compare with `v1` using a **constant-time** comparison (never `==`).
6. Optionally remember `X-Weave-Delivery` ids for the window to reject exact replays.

Retries re-sign with a fresh timestamp, so a delayed retry still passes the window.

### Go

```go
func verify(secret string, header string, body []byte, now time.Time) bool {
	var ts, v1 string
	for _, p := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || math.Abs(float64(now.Unix()-sec)) > 300 {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + string(body)))
	want := hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(want), []byte(v1)) // constant time
}
```

### Node.js

```js
const crypto = require("crypto");

function verify(secret, header, rawBody, nowSec = Math.floor(Date.now() / 1000)) {
  const parts = Object.fromEntries(header.split(",").map((p) => p.trim().split("=", 2)));
  const ts = Number(parts.t);
  if (!Number.isFinite(ts) || Math.abs(nowSec - ts) > 300 || !parts.v1) return false;
  const want = crypto.createHmac("sha256", secret).update(`${parts.t}.${rawBody}`).digest("hex");
  const a = Buffer.from(want), b = Buffer.from(parts.v1);
  return a.length === b.length && crypto.timingSafeEqual(a, b);
}
```

### Python

```python
import hashlib, hmac, time

def verify(secret: str, header: str, raw_body: bytes, now: float | None = None) -> bool:
    parts = dict(p.strip().split("=", 1) for p in header.split(","))
    try:
        ts = int(parts["t"])
    except (KeyError, ValueError):
        return False
    if abs((now or time.time()) - ts) > 300:
        return False
    want = hmac.new(secret.encode(), f"{ts}.".encode() + raw_body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, parts.get("v1", ""))
```

## Test vector

```
secret : whsec_test
t      : 1700000000
body   : {"event":"ping"}
header : t=1700000000,v1=aa8efe37b751e71157c508c5ac4acb1e9fe5225db98355dfc00f4b680afbc447
```

Reproduce: `printf '1700000000.{"event":"ping"}' | openssl dgst -sha256 -hmac whsec_test`.

## Who manages which webhook

| Scope | Created and managed by | Receives |
|---|---|---|
| global (no project, no board) | admins | every event it subscribes to |
| project (`projectId`) | admins | events inside the project |
| board (`boardId`) | anyone who manages the board (board owner, project manager; admins too) | only that board's events, and only board events (`webhook.events scope=board`): cards, columns, labels, comments, tasks, attachments, custom fields, board memberships, board updates. Never `notificationCreate` (that belongs to one user), user, project, configuration, notification-service or webhook events |

Board managers see and manage the board webhooks that non-admins created on their boards (a board webhook an admin created stays
admin-managed), including the delivery log (response bodies are cut to 512 bytes for them; the test ping returns the status and a
256-byte snippet). The access token and signing secret are write-only everywhere. `WEAVE_WEBHOOKS_PER_BOARD` (default 20) caps webhooks per board.

## Cards on several boards

A card can be placed on several boards (placements). A webhook still receives **one delivery per event**:

- An event about the card itself (`cardUpdate`, `commentCreate`, `cardClaim`, `cardRelease`, members, tasks, attachments, card-level
  custom fields, archive/restore/trash moves, `cardDelete`) concerns every board the card is on. A **board** webhook fires when its
  board is one of them, and its payload shows the card **as seen on that board only**: that board's `boardId`, its column and
  position, its labels; never another board's columns, labels or ids. A **project** webhook fires once when any of those boards is in
  its project (the payload is the home board's view when the home is in the project, else the first such board's); a **global** webhook
  fires once with the home board's view.
- An event about one placement (`cardCreate`, a `cardMove` within one board or to another board, `cardPlacementCreate` when a card is
  added to a board, `cardPlacementDelete` when it is removed from one) concerns that board only.
- Label events (`cardLabelCreate`/`cardLabelDelete`) concern the boards that show the label, values of a board-wide custom field only
  that board.

`cardPlacementCreate` and `cardPlacementDelete` are board events (board webhooks may subscribe to them).

## Destinations of board owners' webhooks

A webhook created by a non-admin may not reach the server's own network. Refused: loopback (`127.0.0.0/8`, `::1`, `localhost`),
link-local and cloud metadata (`169.254.0.0/16` incl. `169.254.169.254`, `fe80::/10`, `fd00:ec2::254`, `metadata.google.internal`...),
unspecified, `0.0.0.0/8`, broadcast, multicast, and by default private networks (RFC 1918, ULA `fc00::/7`, CGNAT `100.64.0.0/10`).
The check runs when the URL is saved and again on every connection: Weave resolves the name itself and connects to the address it
checked, so a DNS name that changes in between (rebinding) cannot reach a blocked address; redirects (at most 3, http/https) go
through the same check. No proxy is used. A refused or failed delivery is logged as `could not deliver: destination unreachable or not allowed`.

For receivers on your LAN, an admin turns on **Board webhooks may reach private networks** (Admin console > Settings > Webhooks,
config `webhook.userAllowPrivate`, env `WEAVE_WEBHOOK_USER_ALLOW_PRIVATE=true`). `WEAVE_NOTIFY_ALLOW_HOSTS` allows specific hosts, IPs or
CIDRs regardless of the rules above. Webhooks created by admins are not restricted.
