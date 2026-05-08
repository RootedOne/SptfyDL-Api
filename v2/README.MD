# dl-cli — self-hosted Monochrome download API

A single-binary Go HTTP service that ports the entire browser-side download
pipeline from [Monochrome](../README.md) (`js/api.js`, `js/downloads.js`,
`js/dash-downloader.ts`, `js/hls-downloader.js`) into a stdlib-only Go server.

It can:

- search TIDAL via configured HiFi worker instances,
- resolve a track's stream URL through the same fallback chain the browser
  uses (Qobuz-by-ISRC → TIDAL `/trackManifests` → DASH/HLS/direct),
- download the audio bytes (DASH segment expansion, HLS playlist parsing, or
  plain HTTP), and
- optionally push the finished file to a GitHub repo as an LFS pointer.

There are **no** Python/Node/ffmpeg/spotiflac dependencies; just `go build`.

---

## Table of contents

- [Quick start](#quick-start)
- [Configuration](#configuration)
- [HTTP API](#http-api)
  - [GET /healthz](#get-healthz)
  - [GET /search](#get-search)
  - [POST /dl](#post-dl)
  - [GET /dl/stream](#get-dlstream)
- [End-to-end examples](#end-to-end-examples)
- [How the download pipeline works](#how-the-download-pipeline-works)
- [Quality reference](#quality-reference)
- [Operational notes](#operational-notes)
- [Troubleshooting](#troubleshooting)

---

## Quick start

```bash
# 1. Build (Go ≥ 1.22)
cd dl-cli
go build -buildvcs=false -o monochrome-dl .

# 2. Configure
cp .env.example .env
$EDITOR .env                # at minimum, leave HIFI_INSTANCES alone or update

# 3. Run
set -a; source .env; set +a
./monochrome-dl

# 4. Smoke test
curl http://localhost:8080/healthz
```

Expected output:

```json
{
  "cache_entries": 0,
  "default_quality": "HI_RES_LOSSLESS",
  "github_upload": false,
  "hifi_instances": 2,
  "ok": true,
  "qobuz_instances": 0,
  "search_cache_ttl": "5m0s"
}
```

If you see `"ok": true`, the server is up.

---

## Configuration

Every setting comes from environment variables. See [`.env.example`](.env.example)
for the canonical list with comments. The most-used ones:

| Variable           | Required | Default                         | Description                                                       |
| ------------------ | :------: | ------------------------------- | ----------------------------------------------------------------- |
| `LISTEN_ADDR`      |     ·    | `:8080`                         | TCP bind address.                                                 |
| `HIFI_INSTANCES`   |     ✓    | `https://hifi.geeked.wtf`       | Comma-separated HiFi worker base URLs (TIDAL search & manifests). |
| `QOBUZ_INSTANCES`  |     ·    | _(empty)_                       | Comma-separated Qobuz base URLs for ISRC fallback.                |
| `DEFAULT_QUALITY`  |     ·    | `HI_RES_LOSSLESS`               | Quality when none is given on a request.                          |
| `OUTPUT_DIR`       |     ·    | `$TMPDIR/monochrome-dl`         | Where `/dl` writes files when no `output_dir` is supplied.        |
| `DOWNLOAD_TIMEOUT` |     ·    | `30m`                           | Hard cap on a single `/dl` call.                                  |
| `HTTP_TIMEOUT`     |     ·    | `30s`                           | Per-request timeout for upstream calls.                           |
| `SEARCH_CACHE_TTL` |     ·    | `5m`                            | How long `cache_key`s remain valid.                               |
| `GITHUB_OWNER`     |     ·    | _(empty)_                       | Enables GitHub LFS upload when this + `GITHUB_REPO` + `GITHUB_PAT` are set. |
| `GITHUB_REPO`      |     ·    | _(empty)_                       |                                                                   |
| `GITHUB_BRANCH`    |     ·    | `main`                          |                                                                   |
| `GITHUB_PAT`       |     ·    | _(empty)_                       | PAT with `repo` scope.                                            |

A current list of community HiFi/Qobuz instances lives in the repo's
[INSTANCES.md](../INSTANCES.md).

---

## HTTP API

All endpoints respond with `Content-Type: application/json` (except
`/dl/stream`, which streams audio bytes). Errors use:

```json
{ "error": "human-readable description" }
```

…with an appropriate HTTP status (400 for input problems, 410 for an expired
cache key, 502 for upstream failures, 504 timeout, 500 for internal bugs).

---

### `GET /healthz`

Liveness + config snapshot. Useful for load-balancer probes.

**Request**

```http
GET /healthz HTTP/1.1
```

**200 response**

```json
{
  "cache_entries": 3,
  "default_quality": "HI_RES_LOSSLESS",
  "github_upload": false,
  "hifi_instances": 2,
  "ok": true,
  "qobuz_instances": 1,
  "search_cache_ttl": "5m0s"
}
```

---

### `GET /search`

Searches TIDAL via the configured HiFi instances and caches the result so
that subsequent `/dl` calls can refer to a track by its `number` (1-based).

**Query parameters**

| Name    | Type    | Default | Notes                              |
| ------- | ------- | ------- | ---------------------------------- |
| `q`     | string  | _(req)_ | The search query (artist, title…). |
| `limit` | int     | `10`    | 1 ≤ limit ≤ 50.                    |

**Request**

```http
GET /search?q=daft%20punk%20one%20more%20time&limit=3 HTTP/1.1
```

**200 response**

```json
{
  "cache_key": "ms9q6w8r",
  "items": [
    {
      "NUMBER": 1,
      "ID": "5176085",
      "TRACK": "One More Time",
      "ARTIST": "Daft Punk",
      "ALBUM": "Discovery",
      "COVER_ART": "https://resources.tidal.com/images/3a/4a/.../640x640.jpg",
      "DURATION": 320
    },
    {
      "NUMBER": 2,
      "ID": "5176086",
      "TRACK": "One More Time / Aerodynamic",
      "ARTIST": "Daft Punk",
      "ALBUM": "Alive 2007",
      "COVER_ART": "https://resources.tidal.com/images/.../640x640.jpg",
      "DURATION": 654
    }
  ]
}
```

`cache_key` is opaque (currently a base-36 nano-timestamp) and stays valid for
`SEARCH_CACHE_TTL` after the search call. Pass the pair `(cache_key, number)`
to `/dl`.

**Error examples**

| Status | When                                 | Body                                                |
| ------ | ------------------------------------ | --------------------------------------------------- |
| 400    | `q` is missing or empty              | `{"error":"missing query parameter: q"}`            |
| 502    | every HiFi instance failed / timed out | `{"error":"hifi search: status 502 from https://..."}` |

---

### `POST /dl`

Downloads a track to disk and (optionally) uploads it to GitHub LFS. The
caller must identify the track in **one of two ways**:

1. **By cache key + number** — the legacy contract. Call `/search` first, then
   send the `cache_key` from that response together with the 1-based `number`
   of the row you want.
2. **By direct TIDAL id** — useful when you already know the id and don't want
   to round-trip through search.

**Request body**

```jsonc
{
  "cache_key":  "ms9q6w8r",   // either this+number…
  "number":     1,
  "id":         "5176085",    // …or this. id wins if both are set.
  "quality":    "LOSSLESS",   // optional, defaults to DEFAULT_QUALITY
  "output_dir": "/data/music" // optional, defaults to OUTPUT_DIR
}
```

The file lands at `<output_dir>/<artist>/<title>.<ext>`. The artist/title used
for the path come from the cache (so id-only requests use a placeholder
`Unknown_Artist/track-<id>.<ext>` — search first if you want a clean tree).

**200 response**

```json
{
  "done": true,
  "file_done": true,
  "path": "/data/music/Daft Punk/One More Time.flac",
  "format": "flac",
  "bytes": 38249120,
  "source": "tidal-dash",
  "quality": "LOSSLESS",
  "message": "download complete",
  "gitLink": "https://github.com/your/repo/blob/main/downloads/music/Daft Punk/One More Time.flac"
}
```

| Field      | Notes                                                                                          |
| ---------- | ---------------------------------------------------------------------------------------------- |
| `path`     | Absolute path on disk.                                                                         |
| `format`   | Detected extension (`flac`, `m4a`, `mp3`, `ogg`, `wav`, `mp4`, `ts`).                         |
| `source`   | Which path resolved the URL: `qobuz`, `tidal-dash`, `tidal-hls`, or `tidal-direct`.            |
| `quality`  | Final negotiated quality (may be lower than requested if the format wasn't available).        |
| `gitLink`  | Only present when GitHub upload is configured **and** the upload succeeded.                    |

**Error examples**

| Status | When                                         | Body                                                       |
| ------ | -------------------------------------------- | ---------------------------------------------------------- |
| 400    | Neither `id` nor `cache_key+number` provided | `{"error":"either id or cache_key+number is required"}`    |
| 400    | Body wasn't JSON                             | `{"error":"invalid JSON body"}`                            |
| 410    | Cache key expired                            | `{"error":"cache entry expired; call /search again"}`      |
| 502    | Upstream resolved no usable stream URL       | `{"error":"resolve manifest: malformed track manifest..."}` |

---

### `GET /dl/stream`

Like `/dl` but writes the audio bytes straight back as the response body,
which is convenient for clients that don't share a filesystem with the
server. No GitHub upload is performed on this endpoint.

**Query parameters**

| Name      | Type   | Default           | Notes                       |
| --------- | ------ | ----------------- | --------------------------- |
| `id`      | string | _(req)_           | TIDAL track id.             |
| `quality` | string | `DEFAULT_QUALITY` | See [Quality reference][q]. |

[q]: #quality-reference

**Request**

```http
GET /dl/stream?id=5176085&quality=LOSSLESS HTTP/1.1
```

**200 response**

```http
HTTP/1.1 200 OK
Content-Type: audio/flac
Content-Length: 38249120
Content-Disposition: attachment; filename="5176085.flac"

<binary FLAC bytes>
```

Use it like:

```bash
curl -o "one_more_time.flac" \
  "http://localhost:8080/dl/stream?id=5176085&quality=LOSSLESS"
```

---

## End-to-end examples

### 1. Search → download to disk

```bash
# Step 1 — search and capture the cache_key with jq.
RESP=$(curl -s 'http://localhost:8080/search?q=Aphex%20Twin%20Xtal&limit=3')
echo "$RESP" | jq

CACHE_KEY=$(echo "$RESP" | jq -r .cache_key)

# Step 2 — pick row 1 and download as 16-bit FLAC into /tmp/music.
curl -s -X POST http://localhost:8080/dl \
  -H 'Content-Type: application/json' \
  -d "{
    \"cache_key\": \"$CACHE_KEY\",
    \"number\": 1,
    \"quality\": \"LOSSLESS\",
    \"output_dir\": \"/tmp/music\"
  }" | jq
```

Expected output of the second call:

```json
{
  "done": true,
  "file_done": true,
  "path": "/tmp/music/Aphex Twin/Xtal.flac",
  "format": "flac",
  "bytes": 24117890,
  "source": "tidal-dash",
  "quality": "LOSSLESS",
  "message": "download complete"
}
```

### 2. Stream a known TIDAL id straight to a file

```bash
curl -o "song.flac" \
  "http://localhost:8080/dl/stream?id=5176085&quality=HI_RES_LOSSLESS"

file song.flac
# song.flac: FLAC audio bitstream data, 24 bit, stereo, 96 kHz, ...
```

### 3. Bulk-grab an artist's discography

```bash
ids=(5176085 5176086 5176087)
for id in "${ids[@]}"; do
  curl -s -X POST http://localhost:8080/dl \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$id\",\"quality\":\"LOSSLESS\",\"output_dir\":\"/data/music\"}" \
    | jq -r '.path // .error'
done
```

### 4. Download + auto-upload to a GitHub LFS-backed archive

```bash
export GITHUB_OWNER=you
export GITHUB_REPO=my-music
export GITHUB_PAT=ghp_xxx
./monochrome-dl &

curl -s -X POST http://localhost:8080/dl \
  -H 'Content-Type: application/json' \
  -d '{"id":"5176085","quality":"HI_RES_LOSSLESS"}' | jq
```

```json
{
  "done": true,
  "file_done": true,
  "path": "/tmp/monochrome-dl/Daft Punk/One More Time.flac",
  "format": "flac",
  "bytes": 76542110,
  "source": "tidal-dash",
  "quality": "HI_RES_LOSSLESS",
  "message": "download complete",
  "gitLink": "https://github.com/you/my-music/blob/main/downloads/music/Daft Punk/One More Time.flac"
}
```

### 5. Health-check loop for a process supervisor

```bash
while ! curl -fsS http://localhost:8080/healthz >/dev/null; do
  sleep 1
done
echo "monochrome-dl is up"
```

---

## How the download pipeline works

This server mirrors `LosslessAPI.downloadTrack` from `js/api.js`. For each
`/dl` (or `/dl/stream`) call it executes:

```
┌──────────────────────────────────────────────────────────────────────────┐
│ 1.  GET /info/?id={id}     ── enriches the track (gets ISRC, title…)     │
│        on a HiFi instance                                                │
│                                                                          │
│ 2.  IF the track has an ISRC and QOBUZ_INSTANCES is non-empty:           │
│        for each Qobuz base:                                              │
│          GET /api/get-music?q={ISRC}&offset=0                            │
│          GET /api/download-music?track_id={id}&quality={5|6|27}          │
│        — first non-empty `data.url` wins → "qobuz" source                │
│                                                                          │
│ 3.  ELSE (or Qobuz had no match):                                        │
│        GET /trackManifests/?id={id}&quality={Q}&formats=...              │
│          → JSON:API response with `attributes.uri` (signed URL)          │
│        GET <signed uri>                                                  │
│          → either DASH XML (<MPD>) or JSON `{ urls: [...] }`             │
│                                                                          │
│ 4.  Dispatch by manifest shape:                                          │
│        DASH XML → parse <MPD>, expand SegmentTemplate                    │
│                   ($RepresentationID$/$Number%0Nd$/$Time$),              │
│                   download each segment, concat → "tidal-dash"           │
│        m3u8     → parse master playlist, pick highest BANDWIDTH variant, │
│                   download every segment, concat → "tidal-hls"           │
│        plain    → single HTTP GET → "tidal-direct"                       │
└──────────────────────────────────────────────────────────────────────────┘
```

The HiFi worker fan-out is implemented exactly like the JS code: instances
are tried in order, with status 401/404/429/5xx retrying the next instance.

If you're curious, every step has a corresponding function in `api.go`:

| Step                                  | Go function                                |
| ------------------------------------- | ------------------------------------------ |
| Instance fan-out                      | `fetchHiFi`                                |
| TIDAL search                          | `searchTracks` + `extractItemsArray`       |
| Track metadata / ISRC lookup          | `getTrackMetadata` + `walkForTrack`        |
| Qobuz fallback                        | `tryQobuz`                                 |
| `/trackManifests` + signed manifest   | `fetchTrackManifest` + `findManifestURI`   |
| Stream-URL prioritisation             | `pickBestQualityURL`                       |
| DASH MPD parsing & segment download   | `parseDashManifest` + `downloadDash`       |
| HLS playlist parsing & segment download | `pickHlsVariant` + `downloadHLS`         |
| Top-level orchestration               | `resolveAndDownload`                       |

---

## Quality reference

These map straight to TIDAL's `audioquality` token and the JS
`getTrackManifestFormats(quality)` switch:

| Request value      | Codec / container                                  | Typical bitrate | DASH formats requested  |
| ------------------ | -------------------------------------------------- | --------------- | ----------------------- |
| `HI_RES_LOSSLESS`  | FLAC, 24-bit, ≤ 192 kHz                            | 1411 – 9216 kbps | `FLAC_HIRES`, `FLAC`    |
| `LOSSLESS`         | FLAC, 16-bit, 44.1 kHz                             | ~1411 kbps      | `FLAC`                  |
| `HIGH`             | AAC LC                                             | ~320 kbps       | `AACLC`                 |
| `LOW`              | HE-AAC v1                                          | ~96 kbps        | `HEAACV1`               |
| `DOLBY_ATMOS`      | E-AC-3 with Joint Object Coding (where available)  | ~768 kbps       | `EAC3_JOC`              |

If a track isn't available in the requested format, TIDAL transparently
returns the next-best one and the response's `quality` field reflects what
you actually got.

---

## Operational notes

- **No tagging is performed.** The browser pipeline post-processes audio with
  ffmpeg-wasm and embeds ID3/Vorbis/FLAC tags via `addMetadataToAudio`. This
  Go server skips that step on purpose to stay stdlib-only. If you need
  embedded tags, run `ffmpeg`/`metaflac` over the resulting file:

  ```bash
  metaflac --import-tags-from=tags.txt --import-picture-from=cover.jpg song.flac
  ```

- **Memory footprint.** Audio is buffered in memory before being written to
  disk (or the response body for `/dl/stream`). FLAC `HI_RES_LOSSLESS` tracks
  can be 80 – 200 MB; size your container/VM accordingly.

- **Concurrency.** Each request gets its own `http.Client` request pipeline,
  so calls are independent. Search results live in a per-process map; when
  running multiple replicas behind a load balancer, sticky sessions (or just
  using id-based `/dl` requests) avoid the "cache_key not found" pitfall.

- **DRM.** Tracks with Widevine `drmData` aren't supported; neither are they
  in the browser app for downloads. The default formats requested
  (`FLAC_HIRES` / `FLAC`) are always served unencrypted.

- **Adaptive manifests.** The server requests `adaptive=false` because that's
  what the browser does for downloads (`shouldUseAdaptiveTrackManifest(true)
  → false` in `api.js`).

---

## Troubleshooting

### `hifi search: status 429 from https://hifi.geeked.wtf`

That single instance is rate-limited. Add more URLs to `HIFI_INSTANCES` so
the server can fall through to a fresh one.

### `resolve manifest: malformed track manifest response: …`

The HiFi worker returned something that doesn't include
`attributes.uri`. Either the worker is on an outdated codepath, or the track
id is invalid. Try a different instance or double-check the id with
`curl '<hifi>/info/?id=<id>'`.

### `dash download: segment 0 status 403`

Signed manifest URLs from TIDAL expire (typically ≈ 30 min). If you're
saving manifests and replaying them later, re-fetch instead. For real-time
calls the server already follows the signed URL fresh on every request, so
403s usually mean the upstream instance is misbehaving — try another
`HIFI_INSTANCES` entry.

### `cache entry expired; call /search again`

`SEARCH_CACHE_TTL` elapsed between your `/search` and `/dl`. Either bump the
TTL or pass `id` directly on `/dl`.

### `LFS upload failed status=403 …`

`GITHUB_PAT` lacks `repo` scope, or the repository doesn't have Git LFS
enabled, or the branch is protected. Fix the token / repo settings; the
download itself is unaffected.

### Server isn't crashing but never finishes a download

Check `DOWNLOAD_TIMEOUT` (default 30 min). For very long tracks or slow
upstreams, raise it:

```bash
export DOWNLOAD_TIMEOUT=2h
```

Also bump `HTTP_TIMEOUT` if individual segment fetches are crawling — its
default 30 s applies per segment / per upstream call.

---

## License

Same terms as the [Monochrome project root](../license).
