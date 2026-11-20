// Package synoplay is a fork-only addition (not part of upstream FileBrowser Quantum).
//
// Browsers cannot play containers/codecs such as MPEG-1/2 (.mpg), AVI, WMV or FLV.
// This package converts such a video, on first playback, into an H.264/AAC MP4 kept
// in the cache directory, so the regular (range based) streaming code can serve it.
//
// Like synothumb it only uses the standard library and shells out to ffmpeg/ffprobe.
package synoplay

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultExtensions are the lowercase extensions (without dot) of containers that
// browsers cannot play at all; such files are always converted.
var DefaultExtensions = []string{
	"mpg", "mpeg", "vob", "avi", "wmv", "asf", "flv", "f4v",
	"rm", "rmvb", "ts", "mts", "m2ts", "3gp", "3g2",
}

// DefaultProbeExtensions are containers browsers do play, but only with some codecs.
// An .mp4 holding HEVC (H.265) or 10-bit video plays sound without picture, one with
// AC-3/DTS audio plays picture without sound. These files are inspected with ffprobe
// and converted only when a stream is not browser-compatible.
var DefaultProbeExtensions = []string{"mp4", "m4v", "mov", "mkv"}

// streams describes the first video and audio stream of a file ("" if unknown/absent).
type streams struct {
	video  string // codec name, e.g. "h264", "hevc", "mpeg2video"
	pixFmt string // e.g. "yuv420p", "yuv420p10le"
	audio  string // codec name, e.g. "aac", "ac3"
}

// videoPlayable reports whether browsers decode the video stream as it is.
func (s streams) videoPlayable() bool {
	switch s.video {
	case "h264":
		// Only 8-bit 4:2:0 H.264 is universally supported.
		return s.pixFmt == "" || s.pixFmt == "yuv420p" || s.pixFmt == "yuvj420p"
	case "vp8", "vp9", "av1":
		return true
	}
	return false
}

// audioPlayable reports whether browsers decode the audio stream (or there is none).
func (s streams) audioPlayable() bool {
	switch s.audio {
	case "", "aac", "mp3", "opus", "vorbis", "flac":
		return true
	}
	return false
}

const (
	partialSuffix = ".partial.mp4"
	tmpSuffix     = ".tmp.mp4"
	finalSuffix   = ".mp4"
	// minPartialBytes is how much of an unfinished conversion must exist before it is
	// handed out: enough for the MP4 header and the first fragments.
	minPartialBytes = 256 * 1024
)

// Logf is the logging callback signature.
type Logf func(format string, args ...any)

// Config configures a Manager.
type Config struct {
	FFmpegPath    string   // full path to an ffmpeg binary that has the libx264 and aac encoders
	FFprobePath   string   // full path to ffprobe (optional: without it everything is re-encoded)
	CacheDir      string   // directory for converted files; created if missing
	MaxCacheBytes int64    // delete least recently used conversions beyond this size; <= 0 means unlimited
	Extensions    []string // always converted; defaults to DefaultExtensions
	// ProbeExtensions are converted only when a codec is not browser-compatible;
	// nil means DefaultProbeExtensions, an empty slice disables probing.
	ProbeExtensions []string
	FirstWait       time.Duration // how long a playback request waits for a conversion (default 40s)
	JobTimeout      time.Duration // upper bound for one conversion (default 6h)
	Info            Logf          // optional
	Debug           Logf          // optional
}

type job struct {
	done chan struct{}
	err  error
}

// Manager converts videos one at a time and hands out the playable copies.
type Manager struct {
	cfg       Config
	exts      map[string]struct{}
	probeExts map[string]struct{}

	mu       sync.Mutex
	jobs     map[string]*job   // running or queued, by cache key
	failed   map[string]string // cache key -> error text; not retried until the source changes
	verdicts map[string]bool   // cache key -> "must be converted", for probed containers
	queue    chan func()

	// run executes a command and returns its combined stderr/stdout; replaceable in tests.
	run func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// New prepares the cache directory and starts the single conversion worker.
func New(cfg Config) (*Manager, error) {
	if cfg.FFmpegPath == "" {
		return nil, errors.New("synoplay: ffmpeg path is required")
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("synoplay: cache directory is required")
	}
	// Private: converted copies must not be readable by other local accounts.
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("synoplay: %w", err)
	}
	_ = os.Chmod(cfg.CacheDir, 0o700)
	if len(cfg.Extensions) == 0 {
		cfg.Extensions = DefaultExtensions
	}
	if cfg.ProbeExtensions == nil {
		cfg.ProbeExtensions = DefaultProbeExtensions
	}
	if cfg.FirstWait <= 0 {
		cfg.FirstWait = 40 * time.Second
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = 6 * time.Hour
	}
	if cfg.Info == nil {
		cfg.Info = func(string, ...any) {}
	}
	if cfg.Debug == nil {
		cfg.Debug = func(string, ...any) {}
	}
	m := &Manager{
		cfg:       cfg,
		exts:      map[string]struct{}{},
		probeExts: map[string]struct{}{},
		jobs:      map[string]*job{},
		failed:    map[string]string{},
		verdicts:  map[string]bool{},
		queue:     make(chan func(), 256),
		run:       runCommand,
	}
	for _, e := range cfg.Extensions {
		m.exts[strings.ToLower(strings.TrimPrefix(e, "."))] = struct{}{}
	}
	for _, e := range cfg.ProbeExtensions {
		m.probeExts[strings.ToLower(strings.TrimPrefix(e, "."))] = struct{}{}
	}
	// Leftovers of conversions interrupted by a restart are useless.
	for _, pattern := range []string{"*" + partialSuffix, "*" + tmpSuffix} {
		stale, _ := filepath.Glob(filepath.Join(cfg.CacheDir, pattern))
		for _, f := range stale {
			_ = os.Remove(f)
		}
	}
	go func() {
		for task := range m.queue {
			task()
		}
	}()
	return m, nil
}

// NeedsConversion reports whether a file name has an extension browsers cannot play.
func (m *Manager) NeedsConversion(name string) bool {
	_, ok := m.exts[strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))]
	return ok
}

// ShouldConvert reports whether src must be converted for browser playback: always
// for unplayable containers, and for containers such as MP4 when ffprobe finds a
// video or audio codec browsers cannot decode. The ffprobe verdict is remembered
// until the file changes, so repeated range requests cost nothing.
func (m *Manager) ShouldConvert(ctx context.Context, src string) bool {
	if m.NeedsConversion(src) {
		return true
	}
	if _, probed := m.probeExts[strings.ToLower(strings.TrimPrefix(filepath.Ext(src), "."))]; !probed || m.cfg.FFprobePath == "" {
		return false
	}
	st, err := os.Stat(src)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return false
	}
	key := cacheKey(src, st.Size(), st.ModTime())
	m.mu.Lock()
	verdict, known := m.verdicts[key]
	m.mu.Unlock()
	if known {
		return verdict
	}
	info, ok := m.probe(ctx, src)
	if !ok {
		return false // unreadable or probe interrupted: serve the original, ask again next time
	}
	// Without a video stream there is nothing to fix here (and nothing to map).
	verdict = info.video != "" && (!info.videoPlayable() || !info.audioPlayable())
	m.mu.Lock()
	if len(m.verdicts) > 20000 {
		m.verdicts = map[string]bool{}
	}
	m.verdicts[key] = verdict
	m.mu.Unlock()
	if verdict {
		m.cfg.Debug("video conversion: %s needs converting (video %s %s, audio %s)", src, info.video, info.pixFmt, info.audio)
	}
	return verdict
}

// cacheKey changes whenever the source file changes.
func cacheKey(path string, size int64, mtime time.Time) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d", path, size, mtime.UnixNano())))
	return hex.EncodeToString(sum[:])
}

// Playable returns a browser-playable MP4 for src.
//
// A finished conversion is returned immediately. Otherwise the conversion is started
// (once) and the call waits up to FirstWait: short clips are ready by then. For long
// videos the unfinished file is returned, which plays as far as it has been converted;
// reopening the video later gets more, and finally the complete, seekable file.
//
// ok is false when nothing playable exists (conversion failed or has not produced
// enough yet); the caller then falls back to the original file.
func (m *Manager) Playable(ctx context.Context, src string) (path string, ok bool) {
	st, err := os.Stat(src)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return "", false
	}
	key := cacheKey(src, st.Size(), st.ModTime())
	final := filepath.Join(m.cfg.CacheDir, key+finalSuffix)
	partial := filepath.Join(m.cfg.CacheDir, key+partialSuffix)

	if fileReady(final, 1) {
		now := time.Now()
		_ = os.Chtimes(final, now, now) // mark as recently used for cache eviction
		return final, true
	}

	m.mu.Lock()
	if _, bad := m.failed[key]; bad {
		m.mu.Unlock()
		return "", false
	}
	j, running := m.jobs[key]
	if !running {
		j = &job{done: make(chan struct{})}
		m.jobs[key] = j
		select {
		case m.queue <- func() { m.convert(key, src, partial, final, j) }:
		default: // queue full: give up on this request, a later one will retry
			delete(m.jobs, key)
			m.mu.Unlock()
			return "", false
		}
	}
	m.mu.Unlock()

	timer := time.NewTimer(m.cfg.FirstWait)
	defer timer.Stop()
	select {
	case <-j.done:
		if j.err == nil && fileReady(final, 1) {
			return final, true
		}
		return "", false
	case <-ctx.Done():
	case <-timer.C:
	}
	if fileReady(partial, minPartialBytes) {
		return partial, true
	}
	return "", false
}

func fileReady(path string, minSize int64) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Size() >= minSize
}

func (m *Manager) convert(key, src, partial, final string, j *job) {
	start := time.Now()
	err := m.convertFile(src, partial, final)

	m.mu.Lock()
	delete(m.jobs, key)
	if err != nil {
		m.failed[key] = err.Error()
	}
	m.mu.Unlock()

	if err != nil {
		_ = os.Remove(partial)
		m.cfg.Info("video conversion failed for %s: %v", src, err)
	} else {
		m.cfg.Info("video converted for browser playback in %s: %s", time.Since(start).Round(time.Second), src)
		m.evict(final)
	}
	j.err = err
	close(j.done)
}

func (m *Manager) convertFile(src, partial, final string) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.JobTimeout)
	defer cancel()

	info, _ := m.probe(ctx, src)
	// Streams browsers already decode are copied, which is fast and lossless; VP8 is the
	// exception because it cannot be stored in MP4.
	copyVideo := info.videoPlayable() && info.video != "vp8"
	copyAudio := info.audio == "aac" || info.audio == "mp3"

	err := m.encode(ctx, src, partial, copyVideo, copyAudio, info.audio != "")
	if err != nil && (copyVideo || copyAudio) {
		// Stream copy does not survive every container; re-encode everything instead.
		m.cfg.Debug("video conversion: stream copy failed for %s (%v), re-encoding", src, err)
		_ = os.Remove(partial)
		err = m.encode(ctx, src, partial, false, false, info.audio != "")
	}
	if err != nil {
		return err
	}
	if !fileReady(partial, 1) {
		return errors.New("ffmpeg produced no output")
	}

	// The fragmented file was playable while it grew; rewrite it (no re-encoding) as a
	// regular MP4 with the index up front so that the finished video is fully seekable.
	tmp := strings.TrimSuffix(final, finalSuffix) + tmpSuffix
	out, remuxErr := m.run(ctx, m.cfg.FFmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-protocol_whitelist", "file", "-i", partial, "-map", "0", "-c", "copy", "-movflags", "+faststart", "-f", "mp4", tmp)
	if remuxErr == nil && fileReady(tmp, 1) {
		if err := os.Rename(tmp, final); err != nil {
			return err
		}
		_ = os.Remove(partial)
		return nil
	}
	m.cfg.Debug("video conversion: faststart rewrite failed (%v: %s), keeping fragmented file", remuxErr, tail(out))
	_ = os.Remove(tmp)
	return os.Rename(partial, final)
}

// probe inspects the first video and audio stream with ffprobe. ok is false when
// ffprobe is unavailable or failed.
func (m *Manager) probe(ctx context.Context, src string) (info streams, ok bool) {
	if m.cfg.FFprobePath == "" {
		return info, false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := m.run(ctx, m.cfg.FFprobePath,
		"-v", "error", "-protocol_whitelist", "file",
		"-show_entries", "stream=codec_name,codec_type,pix_fmt", "-of", "default=noprint_wrappers=1", src)
	if err != nil {
		return info, false
	}
	// "key=value" lines; every stream starts with its codec_name.
	var name, kind, pix string
	flush := func() {
		switch {
		case kind == "video" && info.video == "":
			info.video, info.pixFmt = name, pix
		case kind == "audio" && info.audio == "":
			info.audio = name
		}
		name, kind, pix = "", "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "codec_name":
			flush()
			name = value
		case "codec_type":
			kind = value
		case "pix_fmt":
			pix = value
		}
	}
	flush()
	return info, true
}

func (m *Manager) encode(ctx context.Context, src, dst string, copyVideo, copyAudio, hasAudio bool) error {
	// Local files only: a crafted "video" (e.g. a playlist) must not make ffmpeg fetch URLs.
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-protocol_whitelist", "file", "-i", src, "-map", "0:v:0"}
	if hasAudio {
		args = append(args, "-map", "0:a:0")
	} else {
		args = append(args, "-map", "0:a:0?") // codec unknown: take audio if there is any
	}
	args = append(args, "-sn", "-dn")
	if copyVideo {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
			// Deinterlace only frames flagged as interlaced (typical for MPEG-2), cap at 1080p, keep even sizes.
			"-vf", "yadif=mode=0:parity=-1:deint=1,scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2",
		)
	}
	if copyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "160k", "-ac", "2")
	}
	// Fragmented MP4: playable while it is still being written.
	args = append(args, "-movflags", "frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", dst)

	if out, err := m.run(ctx, m.cfg.FFmpegPath, args...); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, tail(out))
	}
	return nil
}

// evict deletes the least recently used finished conversions until the cache fits.
func (m *Manager) evict(keep string) {
	if m.cfg.MaxCacheBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(m.cfg.CacheDir)
	if err != nil {
		return
	}
	type cached struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []cached
	var total int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, finalSuffix) || strings.HasSuffix(name, partialSuffix) || strings.HasSuffix(name, tmpSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, cached{filepath.Join(m.cfg.CacheDir, name), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(a, b int) bool { return files[a].mtime.Before(files[b].mtime) })
	for _, f := range files {
		if total <= m.cfg.MaxCacheBytes {
			return
		}
		if f.path == keep {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
			m.cfg.Debug("video conversion cache: removed %s", f.path)
		}
	}
}

func tail(out []byte) string {
	msg := strings.TrimSpace(string(out))
	if len(msg) > 300 {
		msg = msg[len(msg)-300:]
	}
	return msg
}

func runCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stderr.Bytes(), err
	}
	return stdout.Bytes(), nil
}
