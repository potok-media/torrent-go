package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/bits"
	"os/exec"
	"sync"
	"time"

	"Potok.Backend.TorrentGo/media"
	"Potok.Backend.TorrentGo/storage"
	"github.com/anacrolix/torrent/metainfo"
)

type TimecodeRange struct {
	IntroStart float64 `json:"introStart"`
	IntroEnd   float64 `json:"introEnd"`
	OutroStart float64 `json:"outroStart"`
	OutroEnd   float64 `json:"outroEnd"`
}

type FPCalcOutput struct {
	Duration    float64  `json:"duration"`
	Fingerprint []uint32 `json:"fingerprint"`
}

// AnalyzeTorrent runs in the background and resolves the intro timecodes for all episodes in the torrent
func (h *HandlerContext) AnalyzeTorrent(hashHex string, videoFiles []parsedFile) {
	if len(videoFiles) < 2 {
		slog.Info("Analyzer: skipping torrent, not enough episodes", "hash", hashHex, "count", len(videoFiles))
		return
	}

	if _, ok := h.timecodeCache.Load(hashHex); ok {
		return
	}

	var infoHash metainfo.Hash
	hexBytes, err := hex.DecodeString(hashHex)
	if err == nil && len(hexBytes) == 20 {
		copy(infoHash[:], hexBytes)
		if t, ok := h.Engine.Client.Torrent(infoHash); ok {
			stats := t.Stats()
			if stats.PiecesComplete < t.NumPieces() {
				slog.Info("Analyzer: skipping background analysis because torrent is not fully downloaded", "hash", hashHex, "complete", stats.PiecesComplete, "total", t.NumPieces())
				return
			}
		}
	}

	slog.Info("Analyzer: starting background analysis for torrent", "hash", hashHex, "episodes", len(videoFiles))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 1. Get fingerprints for Episode 1 and Episode 2
	fp1, err := h.getEpisodeFingerprint(ctx, hashHex, videoFiles[0])
	if err != nil {
		slog.Warn("Analyzer: failed to get fingerprint for episode 1", "error", err)
		return
	}

	fp2, err := h.getEpisodeFingerprint(ctx, hashHex, videoFiles[1])
	if err != nil {
		slog.Warn("Analyzer: failed to get fingerprint for episode 2", "error", err)
		return
	}

	// 2. Correlate fingerprints to find the intro segment in Episode 1
	idxStart1, idxEnd1, found := findIntroMatch(fp1, fp2)
	if !found {
		slog.Info("Analyzer: no matching intro sequence found between episode 1 and 2", "hash", hashHex)
		return
	}

	template := fp1[idxStart1 : idxEnd1+1]
	slog.Info("Analyzer: detected intro template", "hash", hashHex, "length_frames", len(template))

	rangesMap := make(map[string]*TimecodeRange)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i, vf := range videoFiles {
		wg.Add(1)
		go func(idx int, file parsedFile) {
			defer wg.Done()

			var epFp []uint32
			var err error

			if idx == 0 {
				epFp = fp1
			} else if idx == 1 {
				epFp = fp2
			} else {
				release, acqErr := h.extExec.Acquire(ctx, extAnalyze)
				if acqErr != nil {
					return
				}
				epFp, err = h.getEpisodeFingerprint(ctx, hashHex, file)
				release()
				if err != nil {
					slog.Warn("Analyzer: failed to get fingerprint for episode", "index", idx, "error", err)
					return
				}
			}

			startSecs, endSecs, matched := matchIntroTemplate(epFp, template)
			if matched {
				mu.Lock()
				rangesMap[file.Item.Id] = &TimecodeRange{
					IntroStart: startSecs,
					IntroEnd:   endSecs,
				}
				mu.Unlock()
				slog.Info("Analyzer: intro detected for episode", "fileIndex", file.Item.Id, "start", startSecs, "end", endSecs)
			} else {
				slog.Info("Analyzer: intro NOT found for episode", "fileIndex", file.Item.Id)
			}
		}(i, vf)
	}

	wg.Wait()

	h.timecodeCache.Store(hashHex, rangesMap)
	slog.Info("Analyzer: successfully cached template-matched intro timecodes for all episodes", "hash", hashHex)
}

const fpSampleRate = 8000 // fingerprint decode rate (mono), matches fpcalc's -rate

func (h *HandlerContext) getEpisodeFingerprint(ctx context.Context, hashHex string, vf parsedFile) ([]uint32, error) {
	fileIndexStr := vf.Item.Id

	// Strict timeout so a cold seek can't hang the background analysis.
	extractCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()

	// Decode the first 8 minutes (480s) of audio to mono S16 PCM IN-PROCESS (libav over the torrent cache,
	// no ffmpeg subprocess / loopback stream), then wrap it in a WAV header and pipe it into fpcalc —
	// Chromaprint has no in-process Go equivalent, so it stays a subprocess, but it now reads a plain
	// in-memory WAV instead of a live ffmpeg pipe.
	rs, _, err := h.openTorrentFileReader(extractCtx, hashHex, fileIndexStr, storage.ClassAheadDemux)
	if err != nil {
		return nil, fmt.Errorf("open torrent file: %w", err)
	}
	defer rs.Close()

	pcm, err := media.AudioPCM(extractCtx, rs, 0, 480, fpSampleRate)
	if err != nil {
		return nil, fmt.Errorf("decode audio: %w", err)
	}
	wav := wrapWAVMono16(pcm, fpSampleRate)

	// fpcalc reads the WAV from stdin and outputs the raw fingerprint as JSON.
	fpcalcCmd := exec.CommandContext(extractCtx, "fpcalc",
		"-rate", "8000",
		"-channels", "1",
		"-length", "480",
		"-raw",
		"-json",
		"-",
	)
	fpcalcCmd.Stdin = bytes.NewReader(wav)
	var fpOutput bytes.Buffer
	fpcalcCmd.Stdout = &fpOutput

	if err := fpcalcCmd.Run(); err != nil {
		return nil, fmt.Errorf("fpcalc failed: %w", err)
	}

	var res FPCalcOutput
	if err := json.Unmarshal(fpOutput.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("failed to parse fpcalc JSON: %w", err)
	}

	return res.Fingerprint, nil
}

// wrapWAVMono16 prepends the canonical 44-byte PCM WAV header to interleaved mono S16LE samples so fpcalc
// (which auto-detects the container) reads them at the right rate/format.
func wrapWAVMono16(pcm []byte, sampleRate int) []byte {
	const (
		channels      = 1
		bitsPerSample = 16
	)
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataLen := len(pcm)

	buf := make([]byte, 0, 44+dataLen)
	buf = append(buf, "RIFF"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(36+dataLen))
	buf = append(buf, "WAVE"...)
	buf = append(buf, "fmt "...)
	buf = binary.LittleEndian.AppendUint32(buf, 16) // PCM fmt chunk size
	buf = binary.LittleEndian.AppendUint16(buf, 1)  // audio format = PCM
	buf = binary.LittleEndian.AppendUint16(buf, channels)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(sampleRate))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(byteRate))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(blockAlign))
	buf = binary.LittleEndian.AppendUint16(buf, bitsPerSample)
	buf = append(buf, "data"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dataLen))
	buf = append(buf, pcm...)
	return buf
}

func findIntroMatch(fp1, fp2 []uint32) (idxStart1, idxEnd1 int, found bool) {
	n := len(fp1)
	m := len(fp2)
	if n == 0 || m == 0 {
		return 0, 0, false
	}

	// 1 frame of AcoustID fingerprint = 124ms (0.124 seconds)
	// 22 seconds of intro = 22 / 0.124 = ~180 frames.
	minMatchFrames := 180

	bestOffset := 0
	bestMatchCount := 0

	// Slide fp2 along fp1
	for offset := -m + minMatchFrames; offset <= n-minMatchFrames; offset++ {
		start := 0
		if offset > 0 {
			start = offset
		}
		start2 := 0
		if offset < 0 {
			start2 = -offset
		}

		overlapLen := n - start
		if m-start2 < overlapLen {
			overlapLen = m - start2
		}

		matchCount := 0
		for i := 0; i < overlapLen; i++ {
			v1 := fp1[start+i]
			v2 := fp2[start2+i]
			// Hamming distance
			dist := bits.OnesCount32(v1 ^ v2)
			if dist <= 6 { // at most 6 bits differ (strong match)
				matchCount++
			}
		}

		if matchCount > bestMatchCount && matchCount >= minMatchFrames {
			bestMatchCount = matchCount
			bestOffset = offset
		}
	}

	if bestMatchCount < minMatchFrames {
		return 0, 0, false
	}

	// Now that we have the bestOffset, let's find the exact start and end of the matching block
	start := 0
	if bestOffset > 0 {
		start = bestOffset
	}
	start2 := 0
	if bestOffset < 0 {
		start2 = -bestOffset
	}

	overlapLen := n - start
	if m-start2 < overlapLen {
		overlapLen = m - start2
	}

	// Scan overlapping frames and mark matches
	matches := make([]bool, overlapLen)
	for i := 0; i < overlapLen; i++ {
		v1 := fp1[start+i]
		v2 := fp2[start2+i]
		if bits.OnesCount32(v1^v2) <= 6 {
			matches[i] = true
		}
	}

	// Find the densest block of matches
	maxDenseStart := -1
	maxDenseEnd := -1
	maxDenseLen := 0

	for i := 0; i < overlapLen; {
		if !matches[i] {
			i++
			continue
		}

		blockStart := i
		consecutiveMismatches := 0
		blockEnd := i

		for j := i; j < overlapLen; j++ {
			if matches[j] {
				blockEnd = j
				consecutiveMismatches = 0
			} else {
				consecutiveMismatches++
				if consecutiveMismatches > 40 { // ~5 seconds of mismatch terminates the block
					break
				}
			}
		}

		blockLen := blockEnd - blockStart
		if blockLen > maxDenseLen && blockLen >= minMatchFrames {
			maxDenseLen = blockLen
			maxDenseStart = blockStart
			maxDenseEnd = blockEnd
		}

		i = blockEnd + 1
	}

	if maxDenseLen >= minMatchFrames {
		idxStart1 = start + maxDenseStart
		idxEnd1 = start + maxDenseEnd

		// Verify the duration is reasonable (30 to 180 seconds)
		durationSecs := float64(idxEnd1-idxStart1) * 0.124
		if durationSecs >= 30 && durationSecs <= 180 {
			return idxStart1, idxEnd1, true
		}
	}

	return 0, 0, false
}

// matchIntroTemplate slides the template along fp and returns the matching timestamps in seconds if matched
func matchIntroTemplate(fp, template []uint32) (start, end float64, found bool) {
	tl := len(template)
	fl := len(fp)
	if tl == 0 || fl < tl {
		return 0, 0, false
	}

	bestScore := 0
	bestStartIdx := -1

	// Slide template along fp
	for offset := 0; offset <= fl-tl; offset++ {
		score := 0
		for j := 0; j < tl; j++ {
			v1 := fp[offset+j]
			v2 := template[j]
			if bits.OnesCount32(v1^v2) <= 6 {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestStartIdx = offset
		}
	}

	if bestStartIdx == -1 {
		return 0, 0, false
	}

	pct := float64(bestScore) / float64(tl)
	if pct >= 0.70 { // 70% frame match threshold
		start = float64(bestStartIdx) * 0.124
		end = float64(bestStartIdx+tl) * 0.124
		return start, end, true
	}

	return 0, 0, false
}
