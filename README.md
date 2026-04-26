# SptfyDL-Api


cat > .env <<'EOF'

SPOTIFY_CLIENT_ID=your_client_id

SPOTIFY_CLIENT_SECRET=your_client_secret

SPOTIFLAC_BIN=/root/SpotiFLAC/spotiflac

SPOTIFLAC_OUTPUT_DIR=/root/downloads

LISTEN_ADDR=:8080

SEARCH_CACHE_TTL=300s

EOF


cd ~/go-api

set -a

source .env

set +a

/usr/local/go/bin/go build -o spotiflac-api spotiflac_api_server.go

./spotiflac-api

