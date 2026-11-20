package files

// Fork-only file (not part of upstream FileBrowser Quantum).
//
// Upstream runs ffprobe on every audio/video file each time a folder's media
// metadata is requested, and the web interface requests it for every folder it
// opens, keeping its loading indicator up until the answer arrives. A folder with
// a few thousand videos therefore spins for tens of seconds on every visit, and
// hits the 60 s request timeout when the NAS is busy.
//
// This file remembers the ffprobe result per file (until its size or mtime
// changes), in memory and in a small JSON file in the cache directory, so the cost
// is paid once. It is reached through a line that synology/apply-source-patches.sh
// rewrites in extractVideoMetadata (files.go) at build time:
//
//	info, err := synologyCachedProbe(ctx, service, realPath)

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gtsteffaniak/filebrowser/backend/internal/ffmpeg"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/settings"
	"github.com/gtsteffaniak/go-logger/logger"
)

const (
	synologyProbeCacheFile  = "synology-probe-cache.json"
	synologyProbeMaxEntries = 500000
	synologyProbeSaveDelay  = 15 * time.Second
	synologyProbeFailureTTL = time.Hour
)

type synologyProbeEntry struct {
	Size       int64   `json:"s"`
	Mtime      int64   `json:"m"` // UnixNano
	Duration   float64 `json:"d,omitempty"`
	VideoCodec string  `json:"v,omitempty"`
	AudioCodec string  `json:"a,omitempty"`
	FormatName string  `json:"f,omitempty"`
}

type synologyProbeCache struct {
	mu       sync.Mutex
	path     string // "" = memory only
	loaded   bool
	entries  map[string]synologyProbeEntry
	failures map[string]time.Time // path -> retry-after; never persisted (may be transient)
	saveDue  bool
}

var synologyProbes = &synologyProbeCache{}

// synologyCachedProbe is service.ProbeFile with a persistent cache in front.
func synologyCachedProbe(ctx context.Context, service *ffmpeg.FFmpegService, realPath string) (*ffmpeg.FileProbeInfo, error) {
	synologyProbes.mu.Lock()
	if !synologyProbes.loaded && synologyProbes.path == "" && settings.Config.Server.CacheDir != "" {
		synologyProbes.path = filepath.Join(settings.Config.Server.CacheDir, synologyProbeCacheFile)
	}
	synologyProbes.mu.Unlock()
	return synologyProbes.get(realPath, func() (*ffmpeg.FileProbeInfo, error) {
		return service.ProbeFile(ctx, realPath)
	})
}

func (c *synologyProbeCache) get(realPath string, probe func() (*ffmpeg.FileProbeInfo, error)) (*ffmpeg.FileProbeInfo, error) {
	st, err := os.Stat(realPath)
	if err != nil {
		return probe()
	}
	size, mtime := st.Size(), st.ModTime().UnixNano()

	c.mu.Lock()
	c.loadLocked()
	if e, ok := c.entries[realPath]; ok && e.Size == size && e.Mtime == mtime {
		c.mu.Unlock()
		return &ffmpeg.FileProbeInfo{Duration: e.Duration, VideoCodec: e.VideoCodec, AudioCodec: e.AudioCodec, FormatName: e.FormatName}, nil
	}
	if until, failed := c.failures[realPath]; failed && time.Now().Before(until) {
		c.mu.Unlock()
		// Known-unreadable file: report "no metadata" instead of probing it on every listing.
		return &ffmpeg.FileProbeInfo{}, nil
	}
	c.mu.Unlock()

	info, err := probe()

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || info == nil {
		c.failures[realPath] = time.Now().Add(synologyProbeFailureTTL)
		return info, err
	}
	if len(c.entries) >= synologyProbeMaxEntries {
		c.entries = map[string]synologyProbeEntry{}
	}
	c.entries[realPath] = synologyProbeEntry{
		Size: size, Mtime: mtime,
		Duration: info.Duration, VideoCodec: info.VideoCodec, AudioCodec: info.AudioCodec, FormatName: info.FormatName,
	}
	delete(c.failures, realPath)
	c.scheduleSaveLocked()
	return info, nil
}

func (c *synologyProbeCache) loadLocked() {
	if c.loaded {
		return
	}
	c.loaded = true
	c.entries = map[string]synologyProbeEntry{}
	c.failures = map[string]time.Time{}
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &c.entries); err != nil || c.entries == nil {
		c.entries = map[string]synologyProbeEntry{}
	}
}

// scheduleSaveLocked writes the cache a little later, so a folder with thousands
// of new entries causes one write instead of thousands.
func (c *synologyProbeCache) scheduleSaveLocked() {
	if c.path == "" || c.saveDue {
		return
	}
	c.saveDue = true
	time.AfterFunc(synologyProbeSaveDelay, c.save)
}

func (c *synologyProbeCache) save() {
	c.mu.Lock()
	c.saveDue = false
	data, err := json.Marshal(c.entries)
	path := c.path
	c.mu.Unlock()
	if err != nil || path == "" {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logger.Debugf("media probe cache: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logger.Debugf("media probe cache: %v", err)
		_ = os.Remove(tmp)
	}
}
