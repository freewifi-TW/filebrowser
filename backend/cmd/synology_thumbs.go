package cmd

// Fork-only file (not part of upstream FileBrowser Quantum).
//
// Starts the Synology "@eaDir" video thumbnail generator when
// FILEBROWSER_SYNOLOGY_THUMBNAILS=true. It hooks in through init() on purpose:
// that way the fork never has to modify an upstream file, so upstream releases
// can be rebuilt with this overlay without merge conflicts.
//
// Environment variables:
//
//	FILEBROWSER_SYNOLOGY_THUMBNAILS           "true" to enable (default: disabled)
//	FILEBROWSER_SYNOLOGY_THUMBNAIL_PATHS      directories to scan, separated by the OS path list
//	                                          separator (":" on Linux). Default: every configured source.
//	FILEBROWSER_SYNOLOGY_THUMBNAIL_INTERVAL   pause between scans as a Go duration (default: 30m)
//	FILEBROWSER_SYNOLOGY_THUMBNAIL_WATCH      "false" to disable reacting to new videos right away

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gtsteffaniak/filebrowser/backend/internal/preview"
	"github.com/gtsteffaniak/filebrowser/backend/internal/synothumb"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/settings"
	"github.com/gtsteffaniak/go-logger/logger"
)

func init() {
	if os.Getenv("FILEBROWSER_SYNOLOGY_THUMBNAILS") != "true" {
		return
	}
	go runSynologyThumbnails()
}

func runSynologyThumbnails() {
	// The preview service is created last during server start-up, after the config
	// was loaded and ffmpeg was probed. CLI sub-commands never create it and exit.
	for preview.GetService() == nil {
		time.Sleep(2 * time.Second)
	}
	time.Sleep(10 * time.Second) // let the first index scan get going

	if !settings.Env.FFmpegAvailable || settings.Env.FFmpegPath == "" {
		logger.Warning("synology thumbnails: enabled but ffmpeg is not available, nothing will be generated")
		return
	}

	roots := filepath.SplitList(os.Getenv("FILEBROWSER_SYNOLOGY_THUMBNAIL_PATHS"))
	if len(roots) == 0 {
		for path, source := range settings.Config.Server.SourceMap {
			if source != nil && source.Config.Disabled {
				continue
			}
			roots = append(roots, path)
		}
	}

	interval := 30 * time.Minute
	if raw := os.Getenv("FILEBROWSER_SYNOLOGY_THUMBNAIL_INTERVAL"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed >= time.Minute {
			interval = parsed
		} else {
			logger.Warningf("synology thumbnails: ignoring invalid FILEBROWSER_SYNOLOGY_THUMBNAIL_INTERVAL %q", raw)
		}
	}

	generator, err := synothumb.New(synothumb.Config{
		FFmpegPath:  executablePath(settings.Env.FFmpegPath, "ffmpeg"),
		FFprobePath: executablePath(settings.Env.FFprobePath, "ffprobe"),
		Roots:       roots,
		Interval:    interval,
		Watch:       os.Getenv("FILEBROWSER_SYNOLOGY_THUMBNAIL_WATCH") != "false",
		MinAge:      2 * time.Minute,
		Info:        logger.Infof,
		Debug:       logger.Debugf,
	})
	if err != nil {
		logger.Warningf("synology thumbnails: %v", err)
		return
	}
	logger.Infof("synology thumbnails: writing video thumbnails to @eaDir for %v (every %s)", roots, interval)
	generator.Run(context.Background())
}

// executablePath accepts either the binary itself or the directory containing it.
func executablePath(path, name string) string {
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
