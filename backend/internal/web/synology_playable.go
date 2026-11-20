package web

// Fork-only file (not part of upstream FileBrowser Quantum).
//
// Lets the media stream endpoint serve a browser-playable MP4 instead of a video
// the browser cannot decode: unplayable containers (MPEG-1/2, AVI, WMV, ...) and
// MP4/MOV/MKV files whose codecs browsers lack (HEVC, 10-bit, AC-3/DTS audio). Enabled with
// FILEBROWSER_SYNOLOGY_TRANSCODE=true.
//
// It is reached through a single line that synology/apply-source-patches.sh inserts
// into ServeSingleFile (stream.go) at build time:
//
//	realPath, displayFileName = synologyPlayable(r, realPath, displayFileName)
//
// Everything after that line - access checks already done, range handling,
// throttling, caching headers - stays upstream's code.
//
// Environment variables:
//
//	FILEBROWSER_SYNOLOGY_TRANSCODE             "true" to enable (default: disabled)
//	FILEBROWSER_SYNOLOGY_TRANSCODE_CACHE_GB    size limit of converted videos in GB (default: 20, 0 = unlimited)
//	FILEBROWSER_SYNOLOGY_TRANSCODE_EXTENSIONS  space or comma separated extensions to convert
//	                                           (default: mpg mpeg vob avi wmv asf flv f4v rm rmvb ts mts m2ts 3gp 3g2)

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/gtsteffaniak/filebrowser/backend/internal/synoplay"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/settings"
	"github.com/gtsteffaniak/go-logger/logger"
)

var (
	synologyPlayOnce    sync.Once
	synologyPlayManager *synoplay.Manager
)

func synologyPlay() *synoplay.Manager {
	synologyPlayOnce.Do(func() {
		if os.Getenv("FILEBROWSER_SYNOLOGY_TRANSCODE") != "true" {
			return
		}
		if !settings.Env.FFmpegAvailable || settings.Env.FFmpegPath == "" {
			logger.Warning("video conversion: enabled but ffmpeg is not available")
			return
		}
		cacheGB := int64(20)
		if raw := os.Getenv("FILEBROWSER_SYNOLOGY_TRANSCODE_CACHE_GB"); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed >= 0 {
				cacheGB = parsed
			}
		}
		var extensions []string
		if raw := os.Getenv("FILEBROWSER_SYNOLOGY_TRANSCODE_EXTENSIONS"); raw != "" {
			extensions = strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == ',' })
		}
		manager, err := synoplay.New(synoplay.Config{
			FFmpegPath:    synologyBinary(settings.Env.FFmpegPath, "ffmpeg"),
			FFprobePath:   synologyBinary(settings.Env.FFprobePath, "ffprobe"),
			CacheDir:      filepath.Join(settings.Config.Server.CacheDir, "synology-playable"),
			MaxCacheBytes: cacheGB << 30,
			Extensions:    extensions,
			Info:          logger.Infof,
			Debug:         logger.Debugf,
		})
		if err != nil {
			logger.Warningf("video conversion disabled: %v", err)
			return
		}
		synologyPlayManager = manager
	})
	return synologyPlayManager
}

// synologyPlayable swaps in the converted MP4 (and an .mp4 display name, which
// decides the Content-Type) for media stream requests of unplayable formats.
// In every other case it returns its arguments unchanged.
func synologyPlayable(r *http.Request, realPath, displayFileName string) (string, string) {
	if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/media/stream") {
		return realPath, displayFileName
	}
	manager := synologyPlay()
	if manager == nil || !manager.ShouldConvert(r.Context(), realPath) {
		return realPath, displayFileName
	}
	playable, ok := manager.Playable(r.Context(), realPath)
	if !ok {
		return realPath, displayFileName
	}
	return playable, strings.TrimSuffix(displayFileName, filepath.Ext(displayFileName)) + ".mp4"
}

// synologyBinary accepts either the binary itself or the directory containing it.
func synologyBinary(path, name string) string {
	if path == "" {
		return ""
	}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		return filepath.Join(path, name)
	}
	return path
}
