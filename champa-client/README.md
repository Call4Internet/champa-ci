# champa-client

AMP-cache domain-fronting tunnel client with adaptive rate control,
cache rotation, and a live status endpoint.

---

## Build (Windows)

Requirements: [Go 1.21+](https://go.dev/dl/) — free, ~130 MB installer.

```
1. Install Go from https://go.dev/dl/
2. Double-click  build.bat
3. champa-client.exe  will appear in the same folder.
```

## Build (Linux / macOS)

```bash
go mod tidy
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o champa-client .
```

---

## Usage

```
champa-client.exe [options] SERVERURL LOCALADDR

Options:
  -pubkey-file  path to server public key file      (required)
  -pubkey       server public key as hex string     (alternative to -pubkey-file)
  -caches       comma-separated AMP cache URLs      (default: Google + Cloudflare)
  -front        domain-fronting host                (e.g. www.google.com)
```

### Recommended (two caches, auto-rotation):

```
champa-client.exe \
    -pubkey-file server.pub \
    -caches "https://cdn.ampproject.org/,https://amp.cloudflare.com/" \
    -front www.google.com \
    https://your-server.example/champa/ 127.0.0.1:7000
```

### Single cache (original behaviour):

```
champa-client.exe \
    -pubkey-file server.pub \
    -caches https://cdn.ampproject.org/ \
    -front www.google.com \
    https://your-server.example/champa/ 127.0.0.1:7000
```

---

## Live status

While the client is running, open a browser or run curl:

```
curl http://127.0.0.1:7001/status
```

Example output:
```json
{
  "session_elapsed_sec": 437.2,
  "conv": "a3f21b8c",
  "is_slow_mode": false,
  "quota_errors": 3,
  "rate_per_sec": 6.0,
  "bytes_per_sec": 45231.0,
  "avg_rtt_ms": 312.0,
  "error_rate": 0.02,
  "max_payload": 3700,
  "concurrency": 4,
  "current_cache": "cdn.ampproject.org"
}
```

### Field reference

| Field | Meaning |
|---|---|
| `session_elapsed_sec` | Seconds since session started |
| `conv` | KCP conversation ID |
| `is_slow_mode` | True when quota back-off is active |
| `quota_errors` | Consecutive quota errors so far |
| `rate_per_sec` | Current poll rate |
| `bytes_per_sec` | Throughput measured last window |
| `avg_rtt_ms` | Average round-trip time |
| `error_rate` | Fraction of failed polls (0–1) |
| `max_payload` | Current outgoing payload size cap |
| `concurrency` | Current in-flight poll limit |
| `current_cache` | Active AMP cache host |

---

## What changed vs original

| Area | Change |
|---|---|
| **Cache rotation** | Client cycles through multiple AMP caches when one hits quota. Google cache and Cloudflare cache have independent quotas, so rotating doubles (or more) the session lifetime. |
| **Session drops** | Previous sessions dropped after ~12 min because a single cache quota window ran out. Rotation keeps the session alive across multiple windows. |
| **Status endpoint** | `http://127.0.0.1:7001/status` shows quota usage, remaining rate, and active cache in real time. |
| **Throughput** | Adaptive controller probes payload size, concurrency, and rate upward when the connection is clean, rather than staying at conservative defaults. |

---

## Proxy setup (SOCKS5 example with ssh)

```
# Forward all traffic through the tunnel
ssh -D 1080 -N -p 7000 user@127.0.0.1
```

Then configure your browser to use SOCKS5 proxy at `127.0.0.1:1080`.
