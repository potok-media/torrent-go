package speed

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

type TorrentSpeed struct {
	DownloadSpeed int64 `json:"downloadSpeed"`
	UploadSpeed   int64 `json:"uploadSpeed"`
}

type Monitor struct {
	client *torrent.Client
	speeds map[string]TorrentSpeed
	mu     sync.RWMutex
}

func NewMonitor(client *torrent.Client) *Monitor {
	return &Monitor{
		client: client,
		speeds: make(map[string]TorrentSpeed),
	}
}

func (m *Monitor) Start(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	type statsSnapshot struct {
		read  int64
		write int64
	}
	lastStats := make(map[string]statsSnapshot)
	tick := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Gather client stats with NO lock held: client.Torrents()/t.Stats() call into the torrent
			// client's own locks, and holding m.mu across them stalls every GetSpeed() (diagnostics /
			// status handlers) for up to a tick. lastStats is owned solely by this goroutine.
			currentTorrents := m.client.Torrents()
			activeHashes := make(map[string]bool, len(currentTorrents))
			newSpeeds := make(map[string]TorrentSpeed, len(currentTorrents))

			for _, t := range currentTorrents {
				h := t.InfoHash().HexString()
				activeHashes[h] = true

				stats := t.Stats()
				currRead := stats.BytesReadUsefulData.Int64()
				currWrite := stats.BytesWrittenData.Int64()

				var dlSpeed, ulSpeed int64
				if last, ok := lastStats[h]; ok {
					dlSpeed = currRead - last.read
					ulSpeed = currWrite - last.write
					if dlSpeed < 0 {
						dlSpeed = 0
					}
					if ulSpeed < 0 {
						ulSpeed = 0
					}
				}
				newSpeeds[h] = TorrentSpeed{DownloadSpeed: dlSpeed, UploadSpeed: ulSpeed}
				lastStats[h] = statsSnapshot{read: currRead, write: currWrite}
			}
			for h := range lastStats {
				if !activeHashes[h] {
					delete(lastStats, h)
				}
			}

			// Publish under the lock — a bare map swap, no client calls held under m.mu.
			m.mu.Lock()
			m.speeds = newSpeeds
			m.mu.Unlock()

			// Aggregate activity snapshot at debug, throttled to ~1 line/15s and only while bytes are
			// actually moving — idle ticks would be pure noise.
			tick++
			if tick >= 15 {
				tick = 0
				var dl, ul int64
				active := 0
				for _, s := range newSpeeds {
					dl += s.DownloadSpeed
					ul += s.UploadSpeed
					if s.DownloadSpeed > 0 || s.UploadSpeed > 0 {
						active++
					}
				}
				if active > 0 {
					slog.Debug("speed", "down_bps", dl, "up_bps", ul, "active_torrents", active, "total_torrents", len(newSpeeds))
				}
			}
		}
	}
}

func (m *Monitor) GetSpeed(hashHex string) TorrentSpeed {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.speeds[hashHex]
}

// Total sums current download/upload speed across every tracked torrent — the aggregate for the
// management dashboard KPIs.
func (m *Monitor) Total() TorrentSpeed {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var t TorrentSpeed
	for _, s := range m.speeds {
		t.DownloadSpeed += s.DownloadSpeed
		t.UploadSpeed += s.UploadSpeed
	}
	return t
}
