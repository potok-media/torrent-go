package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port     int
	LogLevel string
	AuthUser string
	AuthPass string
	// EnableWebUI gates the standalone management web UI + its /api/manage/* control API. Off by
	// default so the admin panel is never exposed unintentionally; the plugin/streaming routes that
	// players and the gateway hit are unaffected. When on, still protect it with POTOK_AUTH_USER/PASS.
	EnableWebUI        bool
	ListenPort         int
	ConnsPerTorrent    int
	HalfOpenConns      int
	CacheSizeBytes     int64 // INITIAL global piece-cache budget across ALL torrents. Runtime-adjustable from the UI (settings.json overrides this on boot); everything else (max concurrent streams, transcoders) is derived from it.
	HlsCacheBytes      int64
	MemLimitBytes      int64
	PreloadBytes       int64
	ThumbCacheSize     int
	ThumbCacheTTL      time.Duration
	TorrentIdleTimeout time.Duration // grace before a torrent with no active playback session is dropped
	SessionTTL         time.Duration // a playback keepalive lapsed longer than this → the session expires
	DisableAnalyzer    bool
	// DownloadDir is where disk-mode torrents persist their pieces (<hash>.dat + .bitmap sidecar). Empty
	// disables disk mode — every torrent falls back to the RAM-only stream cache.
	DownloadDir string
	// DataDir is where the management catalog (metadata + pin/mode) is persisted so it survives restart.
	// Empty keeps the catalog in-memory only (Phase-1 behaviour).
	DataDir string
	// TmdbKey (TMDB_API_KEY, v3 api_key) enables the Add-torrent "fetch from TMDB" lookup. Shared with the
	// gateway via the same env var. Empty disables it — manual title/poster still work.
	TmdbKey string
}

func LoadConfig() *Config {
	cacheMB := getEnvInt64("POTOK_CACHE_SIZE_MB", 256)
	hlsCacheMB := getEnvInt64("POTOK_HLS_CACHE_MB", 256)
	memLimitMB := getEnvInt64("POTOK_MEM_LIMIT_MB", 0) // 0 = no soft limit (respect external GOMEMLIMIT)
	preloadMB := getEnvInt64("POTOK_PRELOAD_MB", 20)

	return &Config{
		Port:               getEnvInt("PORT", 5282),
		LogLevel:           getEnvStr("POTOK_LOG_LEVEL", "info"),
		AuthUser:           getEnvStr("POTOK_AUTH_USER", ""),
		AuthPass:           getEnvStr("POTOK_AUTH_PASS", ""),
		EnableWebUI:        getEnvBool("TORRENTGO_ENABLE_WEBUI", false),
		ListenPort:         getEnvInt("POTOK_LISTEN_PORT", 55123),
		ConnsPerTorrent:    getEnvInt("POTOK_CONNS_PER_TORRENT", 250),
		HalfOpenConns:      getEnvInt("POTOK_HALF_OPEN_CONNS", 120),
		CacheSizeBytes:     cacheMB * 1024 * 1024,
		HlsCacheBytes:      hlsCacheMB * 1024 * 1024,
		MemLimitBytes:      memLimitMB * 1024 * 1024,
		PreloadBytes:       preloadMB * 1024 * 1024,
		ThumbCacheSize:     getEnvInt("POTOK_THUMB_CACHE_SIZE", 200),
		ThumbCacheTTL:      getEnvDuration("POTOK_THUMB_CACHE_TTL", 5*time.Minute),
		TorrentIdleTimeout: getEnvDuration("POTOK_TORRENT_IDLE_TIMEOUT", 60*time.Second),
		SessionTTL:         getEnvDuration("POTOK_SESSION_TTL", 25*time.Second),
		DisableAnalyzer:    getEnvBool("POTOK_DISABLE_ANALYZER", false),
		DownloadDir:        getEnvStr("POTOK_DOWNLOAD_DIR", "downloads"),
		DataDir:            getEnvStr("POTOK_DATA_DIR", ""),
		TmdbKey:            getEnvStr("TMDB_API_KEY", "2c4fa42c601c29b6fea7ad9b211c46f0"),
	}
}

func getEnvStr(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val, ok := os.LookupEnv(key); ok {
		if parsed, err := strconv.Atoi(val); err == nil {
			return parsed
		}
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if val, ok := os.LookupEnv(key); ok {
		if parsed, err := strconv.ParseInt(val, 10, 64); err == nil {
			return parsed
		}
	}
	return defaultVal
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		if parsed, err := time.ParseDuration(val); err == nil {
			return parsed
		}
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		if parsed, err := strconv.ParseBool(val); err == nil {
			return parsed
		}
	}
	return defaultVal
}
