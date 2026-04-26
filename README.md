# SpotiFLAC API

A small Go API that:

* searches Spotify tracks with `/search`
* caches the search results briefly
* downloads a chosen track through the local `spotiflac` binary with `/dl`

## Setup

### 1. Create a `.env` file

In the project folder, create a `.env` file:

```bash
cat > .env <<'EOF'
SPOTIFY_CLIENT_ID=your_client_id
SPOTIFY_CLIENT_SECRET=your_client_secret
SPOTIFLAC_BIN=/root/SpotiFLAC/spotiflac
SPOTIFLAC_OUTPUT_DIR=/root/downloads
LISTEN_ADDR=:8080
SEARCH_CACHE_TTL=300s
EOF
chmod 600 .env
```

### 2. Build the Go binary

Make sure Go is installed and available at `/usr/local/go/bin/go`.

Then build the API:

```bash
cd ~/go-api
set -a
source .env
set +a
/usr/local/go/bin/go build -o spotiflac-api spotiflac_api_server.go
```

## Run the API

### Run in the foreground

```bash
cd ~/go-api
set -a
source .env
set +a
./spotiflac-api
```

## Run in the background and see logs

### Start in background

```bash
cd ~/go-api
set -a
source .env
set +a
nohup ./spotiflac-api > spotiflac-api.log 2>&1 &
```

### See logs live

```bash
tail -f ~/go-api/spotiflac-api.log
```

### Stop the API

```bash
pkill spotiflac-api
```

## API request examples

### 1. Health check

```bash
curl http://localhost:8080/healthz
```

Example response:

```json
{
  "ok": true,
  "spotiflac_bin": "/root/SpotiFLAC/spotiflac",
  "market": "US",
  "search_cache_ttl": "5m0s",
  "cache_entries": 0
}
```

### 2. Search for a song

```bash
curl 'http://localhost:8080/search?q=Blinding%20Lights'
```

Example response:

```json
{
  "cache_key": "dhzgq3pa8c3k",
  "items": [
    {
      "NUMBER": 1,
      "TRACK": "Blinding Lights",
      "ARTIST": "The Weeknd",
      "ALBUM": "After Hours",
      "COVER_ART": "https://i.scdn.co/image/..."
    },
    {
      "NUMBER": 2,
      "TRACK": "Blinding Lights",
      "ARTIST": "The Weeknd",
      "ALBUM": "Blinding Lights",
      "COVER_ART": "https://i.scdn.co/image/..."
    }
  ]
}
```

Save the returned `cache_key`.

### 3. Download a result by number

```bash
curl -X POST 'http://localhost:8080/dl' \
  -H 'Content-Type: application/json' \
  -d '{"cache_key":"dhzgq3pa8c3k","number":1}'
```

Example response:

```json
{
  "done": true,
  "file_done": true,
  "lyric_done": true,
  "message": "download complete"
}
```

### 4. Download with custom options

```bash
curl -X POST 'http://localhost:8080/dl' \
  -H 'Content-Type: application/json' \
  -d '{
    "cache_key":"dhzgq3pa8c3k",
    "number":1,
    "output_dir":"/root/downloads",
    "concurrency":5,
    "delay":"1s"
  }'
```

## Usage flow

1. Call `/search`
2. Copy the returned `cache_key`
3. Pick a result number from `1` to `10`
4. Call `/dl` with that `cache_key` and `number`

## Notes

* If `/dl` says the cache entry expired, run `/search` again and use the new `cache_key`.
* Lyrics are saved when available.
* Keep your `.env` file private because it contains your Spotify client secret.
