# Webhooks: delivery and signature verification

Weave POSTs a JSON body to each matching webhook (`webhook.*` ops, `/admin/webhooks`).
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
