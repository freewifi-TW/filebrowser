// Package synothumb is a fork-only addition (not part of upstream FileBrowser Quantum).
//
// It pre-generates video thumbnails into Synology's "@eaDir" extended-attribute
// directories so that Synology's own apps (File Station, Synology Drive, Synology
// Photos) can show a thumbnail for videos they can no longer decode themselves.
//
// The package deliberately depends on nothing but the standard library: it shells
// out to the ffmpeg/ffprobe binaries directly. That keeps the fork overlay
// compilable against any upstream version.
package synothumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	eaDirName = "@eaDir"
	tmpPrefix = ".fbq-tmp-"
)

// Thumb describes one thumbnail file inside "@eaDir/<video file name>/".
type Thumb struct {
	Name   string // file name inside the per-file @eaDir directory
	Width  int    // bounding box width in pixels
	Height int    // bounding box height in pixels
}

// DefaultThumbs is the set of files written per video. The first entry must be
// the largest: the frame is decoded once at that size and the others are derived
// from it.
//
// None of these names are documented by Synology; they are what DSM itself and
// community tools write:
//   - SYNOPHOTO_THUMB_*            Synology Photos (and reported to be needed on DSM 7.2.2+)
//   - SYNOFILE_THUMB_*             File Station / DS file
//   - SYNOVIDEO_VIDEO_SCREENSHOT   File Station / DS file video thumbnail
var DefaultThumbs = []Thumb{
	{Name: "SYNOPHOTO_THUMB_XL.jpg", Width: 1280, Height: 1280},
	{Name: "SYNOFILE_THUMB_XL.jpg", Width: 1280, Height: 1280},
	{Name: "SYNOVIDEO_VIDEO_SCREENSHOT.jpg", Width: 852, Height: 480},
	{Name: "SYNOFILE_THUMB_M.jpg", Width: 360, Height: 360},
	{Name: "SYNOPHOTO_THUMB_M.jpg", Width: 320, Height: 320},
	{Name: "SYNOPHOTO_THUMB_SM.jpg", Width: 240, Height: 240},
	{Name: "SYNOFILE_THUMB_S.jpg", Width: 120, Height: 120},
}

// DefaultVideoExtensions lists the lowercase extensions (without dot) treated as video.
var DefaultVideoExtensions = []string{
	"mp4", "m4v", "mov", "mkv", "webm", "avi", "wmv", "flv", "f4v", "asf",
	"3gp", "3g2", "ts", "mts", "m2ts", "vob", "mpg", "mpeg", "ogv", "rmvb",
}

// Logf is the logging callback signature used by the generator.
type Logf func(format string, args ...any)

// Config configures a Generator.
type Config struct {
	FFmpegPath  string        // full path to the ffmpeg binary
	FFprobePath string        // full path to the ffprobe binary (optional; without it the frame is taken from the start)
	Roots       []string      // directories to scan recursively
	Interval    time.Duration // pause between full scans; <= 0 means scan once
	Watch       bool          // also react to newly written videos right away (Linux inotify)
	MinAge      time.Duration // skip videos modified more recently than this (still uploading/syncing)
	SeekPercent float64       // position of the captured frame, in percent of the duration
	Timeout     time.Duration // per-ffmpeg-invocation timeout
	Thumbs      []Thumb       // defaults to DefaultThumbs
	Extensions  []string      // defaults to DefaultVideoExtensions
	Info        Logf          // optional
	Debug       Logf          // optional
}

// Stats summarises one scan.
type Stats struct {
	Videos    int // video files seen
	Generated int // videos that got thumbnails written
	UpToDate  int // already had current thumbnails
	Skipped   int // too new, or failed earlier with unchanged mtime
	Failed    int // generation failed in this scan
}

// Generator scans directory trees and writes missing @eaDir thumbnails.
type Generator struct {
	cfg    Config
	exts   map[string]struct{}
	failed map[string]time.Time // path -> video mtime at the time of failure
	now    func() time.Time
	// generate is g.Generate; replaceable in tests.
	generate func(ctx context.Context, videoPath string, videoMtime time.Time) error
	events   <-chan string // paths from the live watcher; nil when not watching
	rescan   bool          // the watcher lost events: scan again without waiting
}

// New validates cfg and returns a Generator.
func New(cfg Config) (*Generator, error) {
	if cfg.FFmpegPath == "" {
		return nil, errors.New("synothumb: ffmpeg path is required")
	}
	if len(cfg.Roots) == 0 {
		return nil, errors.New("synothumb: no directories to scan")
	}
	if len(cfg.Thumbs) == 0 {
		cfg.Thumbs = DefaultThumbs
	}
	if len(cfg.Extensions) == 0 {
		cfg.Extensions = DefaultVideoExtensions
	}
	if cfg.SeekPercent <= 0 || cfg.SeekPercent >= 100 {
		cfg.SeekPercent = 10
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.Info == nil {
		cfg.Info = func(string, ...any) {}
	}
	if cfg.Debug == nil {
		cfg.Debug = func(string, ...any) {}
	}
	exts := make(map[string]struct{}, len(cfg.Extensions))
	for _, e := range cfg.Extensions {
		exts[strings.ToLower(strings.TrimPrefix(e, "."))] = struct{}{}
	}
	g := &Generator{cfg: cfg, exts: exts, failed: map[string]time.Time{}, now: time.Now}
	g.generate = g.Generate
	return g, nil
}

// Run scans all roots, then waits Interval and repeats until ctx is cancelled.
// With Watch enabled, videos written in the meantime are handled within seconds;
// the periodic scan remains as a safety net.
func (g *Generator) Run(ctx context.Context) {
	if g.cfg.Watch {
		g.events = startWatcher(ctx, g)
	}
	for {
		start := g.now()
		g.rescan = false
		stats := g.ScanOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		took := g.now().Sub(start).Round(time.Second)
		if stats.Generated > 0 || stats.Failed > 0 {
			g.cfg.Info("synology thumbnails: scan finished in %s - %d videos, %d generated, %d up to date, %d skipped, %d failed",
				took, stats.Videos, stats.Generated, stats.UpToDate, stats.Skipped, stats.Failed)
		} else {
			g.cfg.Debug("synology thumbnails: scan finished in %s - %d videos, nothing to do", took, stats.Videos)
		}
		if g.cfg.Interval <= 0 && g.events == nil {
			return
		}
		var timer <-chan time.Time // nil (never fires) without an interval
		if g.cfg.Interval > 0 {
			timer = time.After(g.cfg.Interval)
		}
		for wait := !g.rescan; wait; {
			select {
			case <-ctx.Done():
				return
			case <-timer:
				wait = false
			case path := <-g.events:
				g.handleEvent(ctx, path)
				wait = !g.rescan
			}
		}
	}
}

// handleEvent processes one path reported by the watcher. The file was just closed
// by its writer, so unlike the scan it is not held back by MinAge.
func (g *Generator) handleEvent(ctx context.Context, videoPath string) {
	if videoPath == "" {
		g.rescan = true // the watcher lost events
		return
	}
	info, err := os.Lstat(videoPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return
	}
	mtime := info.ModTime()
	if len(g.missing(videoPath, mtime)) == 0 {
		return
	}
	if err := g.generate(ctx, videoPath, mtime); err != nil {
		if ctx.Err() == nil {
			g.failed[videoPath] = mtime
			g.cfg.Debug("synology thumbnails: %s: %v", videoPath, err)
		}
		return
	}
	delete(g.failed, videoPath)
	g.cfg.Debug("synology thumbnails: generated for new video %s", videoPath)
}

// drainEvents handles queued watcher events without blocking, so that a long
// scan does not delay thumbnails for freshly uploaded videos.
func (g *Generator) drainEvents(ctx context.Context) {
	for {
		select {
		case path := <-g.events:
			g.handleEvent(ctx, path)
		default:
			return
		}
	}
}

// ScanOnce walks every root once.
func (g *Generator) ScanOnce(ctx context.Context) Stats {
	var stats Stats
	for _, root := range g.cfg.Roots {
		g.scanRoot(ctx, root, &stats)
	}
	return stats
}

// skipDir reports whether a directory must not be descended into: Synology
// metadata/system folders ("@eaDir", "@tmp", "#recycle", "#snapshot", ...) and hidden folders.
func skipDir(name string) bool {
	return strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#") || strings.HasPrefix(name, ".")
}

func (g *Generator) scanRoot(ctx context.Context, root string, stats *Stats) {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && skipDir(name) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") || !g.isVideo(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 {
			return nil
		}
		stats.Videos++
		g.drainEvents(ctx)
		g.process(ctx, path, info, stats)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		g.cfg.Debug("synology thumbnails: walking %s stopped: %v", root, err)
	}
}

func (g *Generator) isVideo(name string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	_, ok := g.exts[ext]
	return ok
}

// ThumbDir returns the "@eaDir/<file name>" directory for a file.
func ThumbDir(videoPath string) string {
	return filepath.Join(filepath.Dir(videoPath), eaDirName, filepath.Base(videoPath))
}

func (g *Generator) process(ctx context.Context, videoPath string, info fs.FileInfo, stats *Stats) {
	mtime := info.ModTime()
	if len(g.missing(videoPath, mtime)) == 0 {
		stats.UpToDate++
		return
	}
	if g.cfg.MinAge > 0 && g.now().Sub(mtime) < g.cfg.MinAge {
		stats.Skipped++
		return
	}
	if failedAt, ok := g.failed[videoPath]; ok && failedAt.Equal(mtime) {
		stats.Skipped++
		return
	}
	if err := g.generate(ctx, videoPath, mtime); err != nil {
		if ctx.Err() != nil {
			return
		}
		g.failed[videoPath] = mtime
		stats.Failed++
		g.cfg.Debug("synology thumbnails: %s: %v", videoPath, err)
		return
	}
	delete(g.failed, videoPath)
	stats.Generated++
}

// missing returns the thumbnails that do not exist yet or are older than the video.
// Existing current files (possibly made by DSM itself) are never overwritten.
func (g *Generator) missing(videoPath string, videoMtime time.Time) []Thumb {
	dir := ThumbDir(videoPath)
	var out []Thumb
	for _, t := range g.cfg.Thumbs {
		st, err := os.Stat(filepath.Join(dir, t.Name))
		if err != nil || st.Size() == 0 || st.ModTime().Before(videoMtime) {
			out = append(out, t)
		}
	}
	return out
}

// Generate writes the missing or outdated thumbnails for one video.
func (g *Generator) Generate(ctx context.Context, videoPath string, videoMtime time.Time) error {
	todo := g.missing(videoPath, videoMtime)
	if len(todo) == 0 {
		return nil
	}
	dir := ThumbDir(videoPath)
	if err := mkdirAllOpen(dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// 1. Decode one frame, once, at the largest size.
	frame := filepath.Join(dir, tmpPrefix+"frame.jpg")
	defer os.Remove(frame)

	seek := g.seekSeconds(ctx, videoPath)
	err := g.extractFrame(ctx, videoPath, frame, g.cfg.Thumbs[0], seek)
	if err != nil && seek > 0 {
		// Broken duration metadata or unseekable stream: retry from the very start.
		err = g.extractFrame(ctx, videoPath, frame, g.cfg.Thumbs[0], 0)
	}
	if err != nil {
		return err
	}

	// 2. Derive every wanted size from that JPEG (cheap) and publish it atomically.
	// Thumbnails must not look older than the video, even if its mtime is in the future.
	stamp := g.now()
	if videoMtime.After(stamp) {
		stamp = videoMtime
	}
	for _, t := range todo {
		tmp := filepath.Join(dir, tmpPrefix+t.Name)
		if err := g.runFFmpeg(ctx, "-protocol_whitelist", "file", "-i", frame, "-vf", scaleFilter(t), "-q:v", "3", "-f", "mjpeg", tmp); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("resize to %s: %w", t.Name, err)
		}
		_ = os.Chmod(tmp, 0o644)
		_ = os.Chtimes(tmp, stamp, stamp)
		dst := filepath.Join(dir, t.Name)
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("publish %s: %w", dst, err)
		}
	}

	// 3. Drop DSM's "conversion failed" markers so its apps look at the files again.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".fail") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return nil
}

// mkdirAllOpen creates "@eaDir" and the per-file directory world-writable like DSM
// does, so that Synology's own services can keep using them.
//
// Both must be real directories: anyone who can write to the folder could otherwise
// plant a symlink there and make this process write thumbnails (and delete "*.fail"
// files) in a directory of their choosing.
func mkdirAllOpen(dir string) error {
	for _, d := range []string{filepath.Dir(dir), dir} {
		err := os.Mkdir(d, 0o777)
		if err == nil {
			_ = os.Chmod(d, 0o777) // undo umask for directories we just created
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		st, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory (symlink?), refusing to write there", d)
		}
	}
	return nil
}

// scaleFilter fits the frame inside the thumbnail's box, never upscaling, keeping
// the aspect ratio and even dimensions.
func scaleFilter(t Thumb) string {
	w, h := strconv.Itoa(t.Width), strconv.Itoa(t.Height)
	return "scale=w='min(" + w + ",iw)':h='min(" + h + ",ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"
}

func (g *Generator) extractFrame(ctx context.Context, videoPath, dst string, size Thumb, seek float64) error {
	args := []string{}
	if seek > 0 {
		args = append(args, "-ss", strconv.FormatFloat(seek, 'f', 3, 64))
	}
	args = append(args,
		// Local files only: a crafted "video" (e.g. a playlist) must not make ffmpeg fetch URLs.
		"-protocol_whitelist", "file",
		"-i", videoPath,
		"-map", "0:v:0", "-an", "-sn", "-dn",
		"-frames:v", "1",
		"-vf", scaleFilter(size),
		"-q:v", "2",
		"-f", "mjpeg",
		dst,
	)
	if err := g.runFFmpeg(ctx, args...); err != nil {
		return err
	}
	if st, err := os.Stat(dst); err != nil || st.Size() == 0 {
		return errors.New("ffmpeg produced no frame")
	}
	return nil
}

func (g *Generator) runFFmpeg(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	full := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-threads", "1"}, args...)
	cmd := exec.CommandContext(ctx, g.cfg.FFmpegPath, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, msg)
	}
	return nil
}

// seekSeconds returns the capture position, or 0 when the duration is unknown.
func (g *Generator) seekSeconds(ctx context.Context, videoPath string) float64 {
	if g.cfg.FFprobePath == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, g.cfg.FFprobePath,
		"-v", "error", "-protocol_whitelist", "file", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", videoPath).Output()
	if err != nil {
		return 0
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || duration <= 0 {
		return 0
	}
	return duration * g.cfg.SeekPercent / 100
}
