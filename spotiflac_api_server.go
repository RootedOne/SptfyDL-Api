package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	SpotifyClientID     string
	SpotifyClientSecret string
	SpotifyMarket       string
	ListenAddr          string
	SpotiFLACBinary     string
	DefaultOutputDir    string
	DownloadTimeout     time.Duration
	HTTPClientTimeout   time.Duration
	SearchCacheTTL      time.Duration
	GitHubOwner         string
	GitHubRepo          string
	GitHubBranch        string
	GitHubPAT           string
}

type server struct {
	cfg        config
	httpClient *http.Client

	tokenMu     sync.Mutex
	cachedToken string
	tokenExpiry time.Time

	searchMu    sync.Mutex
	searchCache map[string]cachedSearchResults
}

type cachedSearchResults struct {
	Items     []cachedTrackChoice
	ExpiresAt time.Time
	Query     string
	CreatedAt time.Time
}

type cachedTrackChoice struct {
	Number     int    `json:"number"`
	Track      string `json:"track"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	CoverArt   string `json:"cover_art"`
	SpotifyURL string `json:"spotify_url"`
}

type spotifyTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type spotifySearchResponse struct {
	Tracks struct {
		Items []spotifyTrack `json:"items"`
	} `json:"tracks"`
}

type spotifyTrack struct {
	Name         string `json:"name"`
	ExternalURLs struct {
		Spotify string `json:"spotify"`
	} `json:"external_urls"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album struct {
		Name   string `json:"name"`
		Images []struct {
			URL    string `json:"url"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"images"`
	} `json:"album"`
}

type searchItem struct {
	Number   int    `json:"NUMBER"`
	Track    string `json:"TRACK"`
	Artist   string `json:"ARTIST"`
	Album    string `json:"ALBUM"`
	CoverArt string `json:"COVER_ART"`
}

type lyricLine struct {
	Sentence string `json:"sentence"`
	StartSec int    `json:"startSec"`
	EndSec   int    `json:"endSec"`
}

type timedLRCLine struct {
	Timestamp float64
	Sentence  string
}

type downloadRequest struct {
	CacheKey    string `json:"cache_key"`
	Number      int    `json:"number"`
	OutputDir   string `json:"output_dir,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
	Delay       string `json:"delay,omitempty"`
}

type downloadResponse struct {
	Done      bool   `json:"done"`
	FileDone  bool   `json:"file_done"`
	LyricDone bool   `json:"lyric_done"`
	Message   string `json:"message,omitempty"`
	GitLink   string `json:"gitLink,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: cfg.HTTPClientTimeout},
		searchCache: make(map[string]cachedSearchResults),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/dl", s.handleDownload)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.DownloadTimeout + 15*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("server starting listen_addr=%s spotify_market=%s spotiflac_bin=%s search_cache_ttl=%s default_output_dir=%q",
		cfg.ListenAddr, cfg.SpotifyMarket, cfg.SpotiFLACBinary, cfg.SearchCacheTTL, cfg.DefaultOutputDir)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		SpotifyClientID:     strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_ID")),
		SpotifyClientSecret: strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_SECRET")),
		SpotifyMarket:       getenvDefault("SPOTIFY_MARKET", "US"),
		ListenAddr:          getenvDefault("LISTEN_ADDR", ":8080"),
		SpotiFLACBinary:     getenvDefault("SPOTIFLAC_BIN", "spotiflac"),
		DefaultOutputDir:    strings.TrimSpace(os.Getenv("SPOTIFLAC_OUTPUT_DIR")),
		DownloadTimeout:     getenvDuration("DOWNLOAD_TIMEOUT", 60*time.Minute),
		HTTPClientTimeout:   getenvDuration("HTTP_TIMEOUT", 20*time.Second),
		SearchCacheTTL:      getenvDuration("SEARCH_CACHE_TTL", 10*time.Second),
		GitHubOwner:         getenvDefault("GITHUB_OWNER", "dumbowl22"),
		GitHubRepo:          getenvDefault("GITHUB_REPO", "ir-downloader"),
		GitHubBranch:        getenvDefault("GITHUB_BRANCH", "main"),
		GitHubPAT:           strings.TrimSpace(os.Getenv("GITHUB_PAT")),
	}

	if cfg.SpotifyClientID == "" || cfg.SpotifyClientSecret == "" {
		return cfg, errors.New("SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET are required")
	}

	return cfg, nil
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}

	s.searchMu.Lock()
	cacheEntries := len(s.searchCache)
	s.searchMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"spotiflac_bin":    s.cfg.SpotiFLACBinary,
		"market":           s.cfg.SpotifyMarket,
		"search_cache_ttl": s.cfg.SearchCacheTTL.String(),
		"cache_entries":    cacheEntries,
	})
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing query parameter: q"})
		return
	}

	log.Printf("search request query=%q", q)

	items, err := s.searchTracks(r.Context(), q, "10")
	if err != nil {
		log.Printf("search failed query=%q err=%v", q, err)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})
		return
	}

	cacheKey := s.storeSearchResults(q, items)

	responseItems := make([]searchItem, 0, len(items))
	for _, item := range items {
		responseItems = append(responseItems, searchItem{
			Number:   item.Number,
			Track:    item.Track,
			Artist:   item.Artist,
			Album:    item.Album,
			CoverArt: item.CoverArt,
		})
	}

	log.Printf("search success query=%q cache_key=%s results=%d", q, cacheKey, len(responseItems))

	writeJSON(w, http.StatusOK, map[string]any{
		"cache_key": cacheKey,
		"items":     responseItems,
	})
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}

	var req downloadRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
		return
	}

	req.CacheKey = strings.TrimSpace(req.CacheKey)
	req.OutputDir = strings.TrimSpace(req.OutputDir)
	req.Delay = strings.TrimSpace(req.Delay)

	if req.CacheKey == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "cache_key is required"})
		return
	}
	if req.Number < 1 || req.Number > 10 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "number must be between 1 and 10"})
		return
	}

	log.Printf("download request cache_key=%s number=%d output_dir=%q concurrency=%d delay=%q",
		req.CacheKey, req.Number, req.OutputDir, req.Concurrency, req.Delay)

	choice, err := s.lookupCachedChoice(req.CacheKey, req.Number)
	if err != nil {
		log.Printf("download cache lookup failed cache_key=%s number=%d err=%v", req.CacheKey, req.Number, err)
		writeJSON(w, http.StatusGone, errorResponse{Error: err.Error()})
		return
	}

	outputDir := req.OutputDir
	if outputDir == "" {
		outputDir = s.cfg.DefaultOutputDir
	}

	resp, err := s.downloadWithSpotiFLAC(r.Context(), req, outputDir, choice)
	if err != nil {
		status := http.StatusBadGateway
		var execErr *exec.ExitError
		if errors.As(err, &execErr) {
			status = http.StatusInternalServerError
		}
		log.Printf("download failed cache_key=%s number=%d spotify_url=%s err=%v", req.CacheKey, req.Number, choice.SpotifyURL, err)
		writeJSON(w, status, errorResponse{Error: err.Error()})
		return
	}

	log.Printf("download success cache_key=%s number=%d spotify_url=%s file_done=%v lyric_done=%v",
		req.CacheKey, req.Number, choice.SpotifyURL, resp.FileDone, resp.LyricDone)
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) searchTracks(ctx context.Context, query, limit string) ([]cachedTrackChoice, error) {
	token, err := s.getSpotifyToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get spotify token: %w", err)
	}

	endpoint, err := url.Parse("https://api.spotify.com/v1/search")
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("type", "track")
	params.Set("limit", limit)
	if s.cfg.SpotifyMarket != "" {
		params.Set("market", s.cfg.SpotifyMarket)
	}
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spotify search failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed spotifySearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode spotify response: %w", err)
	}

	items := make([]cachedTrackChoice, 0, len(parsed.Tracks.Items))
	for i, t := range parsed.Tracks.Items {
		items = append(items, cachedTrackChoice{
			Number:     i + 1,
			Track:      t.Name,
			Artist:     joinArtists(t.Artists),
			Album:      t.Album.Name,
			CoverArt:   largestImageURL(t.Album.Images),
			SpotifyURL: t.ExternalURLs.Spotify,
		})
	}

	return items, nil
}

func (s *server) getSpotifyToken(ctx context.Context) (string, error) {
	s.tokenMu.Lock()
	if s.cachedToken != "" && time.Until(s.tokenExpiry) > 30*time.Second {
		token := s.cachedToken
		expiresIn := time.Until(s.tokenExpiry).Round(time.Second)
		s.tokenMu.Unlock()
		log.Printf("spotify token cache hit expires_in=%s", expiresIn)
		return token, nil
	}
	s.tokenMu.Unlock()

	log.Printf("spotify token cache miss requesting new token")

	form := url.Values{}
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://accounts.spotify.com/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+basicAuth(s.cfg.SpotifyClientID, s.cfg.SpotifyClientSecret))

	res, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spotify token request failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var tok spotifyTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New("spotify token response missing access_token")
	}

	expiry := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)

	s.tokenMu.Lock()
	s.cachedToken = tok.AccessToken
	s.tokenExpiry = expiry
	s.tokenMu.Unlock()

	log.Printf("spotify token refreshed expires_in=%ds", tok.ExpiresIn)
	return tok.AccessToken, nil
}

func (s *server) storeSearchResults(query string, items []cachedTrackChoice) string {
	s.searchMu.Lock()
	defer s.searchMu.Unlock()

	s.pruneExpiredLocked()

	cacheKey := strconv.FormatInt(time.Now().UnixNano(), 36)
	s.searchCache[cacheKey] = cachedSearchResults{
		Items:     items,
		ExpiresAt: time.Now().Add(s.cfg.SearchCacheTTL),
		Query:     query,
		CreatedAt: time.Now(),
	}

	log.Printf("search cache store cache_key=%s query=%q items=%d expires_at=%s",
		cacheKey, query, len(items), s.searchCache[cacheKey].ExpiresAt.Format(time.RFC3339))

	return cacheKey
}

func (s *server) lookupCachedChoice(cacheKey string, number int) (cachedTrackChoice, error) {
	s.searchMu.Lock()
	defer s.searchMu.Unlock()

	s.pruneExpiredLocked()

	entry, ok := s.searchCache[cacheKey]
	if !ok {
		return cachedTrackChoice{}, errors.New("cache entry not found or expired; call /search again")
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(s.searchCache, cacheKey)
		return cachedTrackChoice{}, errors.New("cache entry expired; call /search again")
	}

	for _, item := range entry.Items {
		if item.Number == number {
			log.Printf("search cache hit cache_key=%s query=%q number=%d spotify_url=%s",
				cacheKey, entry.Query, number, item.SpotifyURL)
			return item, nil
		}
	}

	return cachedTrackChoice{}, errors.New("number not found in cached search results")
}

func (s *server) pruneExpiredLocked() {
	now := time.Now()
	for key, entry := range s.searchCache {
		if now.After(entry.ExpiresAt) {
			log.Printf("search cache evict cache_key=%s query=%q created_at=%s expired_at=%s",
				key, entry.Query, entry.CreatedAt.Format(time.RFC3339), entry.ExpiresAt.Format(time.RFC3339))
			delete(s.searchCache, key)
		}
	}
}

func (s *server) downloadWithSpotiFLAC(parent context.Context, req downloadRequest, outputDir string, choice cachedTrackChoice) (downloadResponse, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.DownloadTimeout)
	defer cancel()

	args := []string{}
	if outputDir != "" {
		cleaned, err := filepath.Abs(outputDir)
		if err != nil {
			return downloadResponse{}, fmt.Errorf("invalid output_dir: %w", err)
		}
		if err := os.MkdirAll(cleaned, 0o755); err != nil {
			return downloadResponse{}, fmt.Errorf("create output_dir: %w", err)
		}
		outputDir = cleaned
		args = append(args, "-o", outputDir)
	}
	if req.Concurrency > 0 {
		args = append(args, "-c", fmt.Sprintf("%d", req.Concurrency))
	}
	if req.Delay != "" {
		if _, err := time.ParseDuration(req.Delay); err != nil {
			return downloadResponse{}, fmt.Errorf("invalid delay: %w", err)
		}
		args = append(args, "-delay", req.Delay)
	}
	args = append(args, choice.SpotifyURL)

	log.Printf("spotiflac exec start cmd=%q args=%q", s.cfg.SpotiFLACBinary, args)

	cmd := exec.CommandContext(ctx, s.cfg.SpotiFLACBinary, args...)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)

	if ctx.Err() == context.DeadlineExceeded {
		log.Printf("spotiflac exec timeout spotify_url=%s after=%s", choice.SpotifyURL, s.cfg.DownloadTimeout)
		return downloadResponse{}, fmt.Errorf("spotiflac timed out after %s", s.cfg.DownloadTimeout)
	}
	if err != nil {
		log.Printf("spotiflac exec failed spotify_url=%s duration=%s stdout_len=%d stderr_len=%d err=%v",
			choice.SpotifyURL, elapsed, stdout.Len(), stderr.Len(), err)
		return downloadResponse{}, fmt.Errorf("spotiflac failed: %w", err)
	}

	lyrics := parseLyricsFromStdout(stdout.String())
	lyricDone := false
	if len(lyrics) > 0 {
		if _, saveErr := saveLyricsJSON(stdout.String(), outputDir, lyrics); saveErr != nil {
			log.Printf("lyrics save failed spotify_url=%s err=%v", choice.SpotifyURL, saveErr)
		} else {
			lyricDone = true
			log.Printf("lyrics saved spotify_url=%s lines=%d", choice.SpotifyURL, len(lyrics))
		}
	} else if strings.Contains(stdout.String(), "Lyrics embedded successfully!") || strings.Contains(stdout.String(), "LYRICS FETCH END (SUCCESS)") {
		lyricDone = true
	}

	log.Printf("spotiflac exec success spotify_url=%s duration=%s file_done=true lyric_done=%v",
		choice.SpotifyURL, elapsed, lyricDone)

	// Determine local audio file path
	audioPath := extractAudioFilePath(stdout.String())

	if audioPath == "" {
		log.Printf("Could not extract audio file path from spotiflac output, attempting fallback search in %s", outputDir)
		var latestFile string
		var latestTime time.Time

		err := filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				ext := strings.ToLower(filepath.Ext(path))
				if ext == ".flac" || ext == ".mp3" || ext == ".m4a" || ext == ".wav" || ext == ".ogg" {
					if info.ModTime().After(started) && info.ModTime().After(latestTime) {
						latestTime = info.ModTime()
						latestFile = path
					}
				}
			}
			return nil
		})

		if err != nil {
			log.Printf("Fallback search for audio file failed: %v", err)
		} else if latestFile != "" {
			audioPath = latestFile
			log.Printf("Fallback search found audio file: %s", audioPath)
		} else {
			log.Printf("Fallback search found no recently created audio files in %s", outputDir)
		}
	}

	var lyricsPath string
	if audioPath != "" {
		base := strings.TrimSuffix(audioPath, filepath.Ext(audioPath))
		possibleLyrics := base + ".lyrics.json"
		if _, err := os.Stat(possibleLyrics); err == nil {
			lyricsPath = possibleLyrics
		}
	}

	gitLink := ""
	if audioPath != "" {
		sanitizedArtist := sanitizePath(choice.Artist)
		if sanitizedArtist == "" {
			sanitizedArtist = "Unknown_Artist"
		}
		audioFileName := filepath.Base(audioPath)
		gitHubAudioPath := fmt.Sprintf("downloads/music/%s/%s", sanitizedArtist, audioFileName)

		commitMsg := fmt.Sprintf("Auto-upload: %s - Audio & Lyrics", choice.Track)

		err := uploadToGitHubLFS(ctx, s, audioPath, gitHubAudioPath, commitMsg)
		if err != nil {
			log.Printf("GitHub LFS upload failed for audio %s: %v", audioPath, err)
		} else {
			gitLink = fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", s.cfg.GitHubOwner, s.cfg.GitHubRepo, s.cfg.GitHubBranch, gitHubAudioPath)
			log.Printf("GitHub LFS upload success for audio %s: %s", audioPath, gitLink)
		}

		if lyricsPath != "" {
			lyricsFileName := filepath.Base(lyricsPath)
			gitHubLyricsPath := fmt.Sprintf("downloads/music/%s/%s", sanitizedArtist, lyricsFileName)
			lyricsBytes, err := os.ReadFile(lyricsPath)
			if err != nil {
				log.Printf("Failed to read lyrics file %s: %v", lyricsPath, err)
			} else {
				err = uploadToGitHubAPI(ctx, s, lyricsBytes, gitHubLyricsPath, commitMsg)
				if err != nil {
					log.Printf("GitHub API upload failed for lyrics %s: %v", lyricsPath, err)
				} else {
					log.Printf("GitHub API upload success for lyrics %s", lyricsPath)
				}
			}
		}
	}

	return downloadResponse{
		Done:      true,
		FileDone:  true,
		LyricDone: lyricDone,
		Message:   "download complete",
		GitLink:   gitLink,
	}, nil
}

func parseLyricsFromStdout(stdout string) []lyricLine {
	block := extractLRCBlock(stdout)
	if block == "" {
		return nil
	}

	timed := parseTimedLRC(block)
	if len(timed) == 0 {
		return nil
	}

	lyrics := make([]lyricLine, 0, len(timed))
	for i, line := range timed {
		start := int(line.Timestamp)
		end := start + 5
		if i+1 < len(timed) {
			next := int(timed[i+1].Timestamp)
			if next > start {
				end = next
			}
		}
		if end <= start {
			end = start + 1
		}
		lyrics = append(lyrics, lyricLine{
			Sentence: line.Sentence,
			StartSec: start,
			EndSec:   end,
		})
	}

	return lyrics
}

func extractLRCBlock(stdout string) string {
	startMarker := "--- Full LRC Content ---"
	endMarker := "--- End LRC Content ---"

	start := strings.Index(stdout, startMarker)
	if start == -1 {
		return ""
	}
	start += len(startMarker)

	end := strings.Index(stdout[start:], endMarker)
	if end == -1 {
		return ""
	}

	block := stdout[start : start+end]
	return strings.TrimSpace(block)
}

func parseTimedLRC(block string) []timedLRCLine {
	re := regexp.MustCompile(`^\[(\d{2}):(\d{2})(?:\.(\d{1,2}))?\](.*)$`)
	lines := strings.Split(block, "\n")
	parsed := make([]timedLRCLine, 0, len(lines))

	for _, raw := range lines {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[ti:") || strings.HasPrefix(line, "[ar:") || strings.HasPrefix(line, "[by:") {
			continue
		}

		m := re.FindStringSubmatch(line)
		if len(m) == 0 {
			continue
		}

		minute, err1 := strconv.Atoi(m[1])
		second, err2 := strconv.Atoi(m[2])
		if err1 != nil || err2 != nil {
			continue
		}

		fraction := 0.0
		if m[3] != "" {
			fracRaw := m[3]
			if len(fracRaw) == 1 {
				fracRaw += "0"
			}
			fracInt, err := strconv.Atoi(fracRaw)
			if err == nil {
				fraction = float64(fracInt) / 100.0
			}
		}

		sentence := strings.TrimSpace(m[4])
		if sentence == "" {
			continue
		}

		parsed = append(parsed, timedLRCLine{
			Timestamp: float64(minute*60+second) + fraction,
			Sentence:  sentence,
		})
	}

	sort.Slice(parsed, func(i, j int) bool {
		return parsed[i].Timestamp < parsed[j].Timestamp
	})

	return parsed
}

func saveLyricsJSON(stdout, outputDir string, lyrics []lyricLine) (string, error) {
	audioPath := extractAudioFilePath(stdout)
	if audioPath == "" {
		name := fmt.Sprintf("lyrics_%d.json", time.Now().Unix())
		if outputDir == "" {
			outputDir = "."
		}
		audioPath = filepath.Join(outputDir, name)
	}

	base := strings.TrimSuffix(audioPath, filepath.Ext(audioPath))
	lyricsPath := base + ".lyrics.json"

	data, err := json.MarshalIndent(lyrics, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	if err := os.WriteFile(lyricsPath, data, 0o644); err != nil {
		return "", err
	}

	return lyricsPath, nil
}

func extractAudioFilePath(stdout string) string {
	markers := []string{
		"Embedding into:",
		"Downloading to:",
	}

	for _, marker := range markers {
		idx := strings.LastIndex(stdout, marker)
		if idx == -1 {
			continue
		}

		line := stdout[idx+len(marker):]
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
		}

		path := strings.TrimSpace(line)
		if path != "" {
			return path
		}
	}

	return ""
}

func joinArtists(artists []struct{ Name string `json:"name"` }) string {
	names := make([]string, 0, len(artists))
	for _, a := range artists {
		if strings.TrimSpace(a.Name) != "" {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}

func largestImageURL(images []struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}) string {
	if len(images) == 0 {
		return ""
	}

	best := images[0]
	bestArea := best.Width * best.Height

	for _, img := range images[1:] {
		area := img.Width * img.Height
		if area > bestArea {
			best = img
			bestArea = area
		}
	}

	return best.URL
}

func basicAuth(username, password string) string {
	raw := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func getenvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}
		next.ServeHTTP(rec, r)
		log.Printf("http request method=%s path=%s raw_query=%q remote=%s status=%d duration=%s",
			r.Method, r.URL.Path, r.URL.RawQuery, r.RemoteAddr, rec.status, time.Since(started))
	})
}

// GitHub Helper Functions

func sanitizePath(name string) string {
	// Remove invalid path characters
	re := regexp.MustCompile(`[<>:"/\\|?*]`)
	sanitized := re.ReplaceAllString(name, "")
	return strings.TrimSpace(sanitized)
}

type lfsBatchRequest struct {
	Operation string `json:"operation"`
	Transfers []string `json:"transfers"`
	Ref struct {
		Name string `json:"name"`
	} `json:"ref"`
	Objects []lfsObject `json:"objects"`
}

type lfsObject struct {
	Oid string `json:"oid"`
	Size int64 `json:"size"`
}

type lfsBatchResponse struct {
	Objects []struct {
		Oid string `json:"oid"`
		Size int64 `json:"size"`
		Actions struct {
			Upload struct {
				Href string `json:"href"`
				Header map[string]string `json:"header"`
				ExpiresIn int `json:"expires_in"`
			} `json:"upload"`
		} `json:"actions"`
		Error *struct {
			Code int `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"objects"`
}

type gitHubContentRequest struct {
	Message string `json:"message"`
	Content string `json:"content"`
	Branch string `json:"branch,omitempty"`
	Sha string `json:"sha,omitempty"`
}

type gitHubContentResponse struct {
	Sha string `json:"sha"`
}

func uploadToGitHubLFS(ctx context.Context, s *server, localAudioPath, gitHubPath, commitMsg string) error {
	file, err := os.Open(localAudioPath)
	if err != nil {
		return fmt.Errorf("failed to open local audio file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat local audio file: %w", err)
	}
	size := stat.Size()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("failed to hash local audio file: %w", err)
	}
	oid := hex.EncodeToString(hash.Sum(nil))

	// Reset file pointer for uploading later
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek file: %w", err)
	}

	// 1. Make LFS Batch Request
	batchReq := lfsBatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects: []lfsObject{
			{Oid: oid, Size: size},
		},
	}
	batchReq.Ref.Name = "refs/heads/" + s.cfg.GitHubBranch

	batchBody, err := json.Marshal(batchReq)
	if err != nil {
		return err
	}

	batchURL := fmt.Sprintf("https://github.com/%s/%s.git/info/lfs/objects/batch", s.cfg.GitHubOwner, s.cfg.GitHubRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, batchURL, bytes.NewReader(batchBody))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.git-lfs+json")
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	req.Header.Set("Authorization", "token "+s.cfg.GitHubPAT)

	res, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("LFS batch request failed: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("LFS batch request failed with status %d: %s", res.StatusCode, string(body))
	}

	var batchResp lfsBatchResponse
	if err := json.NewDecoder(res.Body).Decode(&batchResp); err != nil {
		return fmt.Errorf("failed to decode LFS batch response: %w", err)
	}

	if len(batchResp.Objects) == 0 {
		return errors.New("no objects returned in LFS batch response")
	}

	obj := batchResp.Objects[0]
	if obj.Error != nil {
		return fmt.Errorf("LFS object error: %s (code %d)", obj.Error.Message, obj.Error.Code)
	}

	// If there's an upload action, upload the file
	if obj.Actions.Upload.Href != "" {
		uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPut, obj.Actions.Upload.Href, file)
		if err != nil {
			return fmt.Errorf("failed to create LFS upload request: %w", err)
		}
		uploadReq.ContentLength = size

		for k, v := range obj.Actions.Upload.Header {
			uploadReq.Header.Set(k, v)
		}

		// Some implementations require content-type, but basic transfer says to send raw binary
		if uploadReq.Header.Get("Content-Type") == "" {
			uploadReq.Header.Set("Content-Type", "application/octet-stream")
		}

		uploadRes, err := s.httpClient.Do(uploadReq)
		if err != nil {
			return fmt.Errorf("LFS upload failed: %w", err)
		}
		defer uploadRes.Body.Close()

		if uploadRes.StatusCode >= 300 {
			body, _ := io.ReadAll(uploadRes.Body)
			return fmt.Errorf("LFS upload failed with status %d: %s", uploadRes.StatusCode, string(body))
		}
	}

	// 2. Create LFS Pointer File and commit it via GitHub API
	pointerFileContent := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)

	return uploadToGitHubAPI(ctx, s, []byte(pointerFileContent), gitHubPath, commitMsg)
}

func uploadToGitHubAPI(ctx context.Context, s *server, contentBytes []byte, gitHubPath, commitMsg string) error {
	encodedContent := base64.StdEncoding.EncodeToString(contentBytes)

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", s.cfg.GitHubOwner, s.cfg.GitHubRepo, gitHubPath)

	// Check if file exists to get SHA (for updates)
	var existingSha string
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"?ref="+s.cfg.GitHubBranch, nil)
	if err == nil {
		getReq.Header.Set("Authorization", "token "+s.cfg.GitHubPAT)
		getReq.Header.Set("Accept", "application/vnd.github.v3+json")
		getRes, err := s.httpClient.Do(getReq)
		if err == nil && getRes.StatusCode == http.StatusOK {
			defer getRes.Body.Close()
			var existing gitHubContentResponse
			if err := json.NewDecoder(getRes.Body).Decode(&existing); err == nil {
				existingSha = existing.Sha
			}
		} else if getRes != nil {
			getRes.Body.Close()
		}
	}

	reqBody := gitHubContentRequest{
		Message: commitMsg,
		Content: encodedContent,
		Branch:  s.cfg.GitHubBranch,
		Sha:     existingSha,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	putReq.Header.Set("Authorization", "token "+s.cfg.GitHubPAT)
	putReq.Header.Set("Accept", "application/vnd.github.v3+json")
	putReq.Header.Set("Content-Type", "application/json")

	putRes, err := s.httpClient.Do(putReq)
	if err != nil {
		return fmt.Errorf("GitHub API PUT failed: %w", err)
	}
	defer putRes.Body.Close()

	if putRes.StatusCode != http.StatusCreated && putRes.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(putRes.Body)
		return fmt.Errorf("GitHub API PUT failed with status %d: %s", putRes.StatusCode, string(respBody))
	}

	return nil
}
