package files

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gtsteffaniak/filebrowser/backend/internal/ffmpeg"
)

func TestSynologyProbeCache(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(video, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, "probe.json")

	calls := 0
	probe := func() (*ffmpeg.FileProbeInfo, error) {
		calls++
		return &ffmpeg.FileProbeInfo{Duration: 12.5, VideoCodec: "h264", AudioCodec: "aac", FormatName: "mp4"}, nil
	}

	c := &synologyProbeCache{path: cachePath}
	for i := 0; i < 3; i++ {
		info, err := c.get(video, probe)
		if err != nil || info.Duration != 12.5 || info.VideoCodec != "h264" {
			t.Fatalf("get #%d = %+v, %v", i, info, err)
		}
	}
	if calls != 1 {
		t.Fatalf("probed %d times for an unchanged file, want 1", calls)
	}

	// Survives a restart through the file on disk.
	c.save()
	restarted := &synologyProbeCache{path: cachePath}
	if info, err := restarted.get(video, probe); err != nil || info.AudioCodec != "aac" || calls != 1 {
		t.Fatalf("after reload: %+v, %v, %d probes", info, err, calls)
	}

	// A modified file is probed again.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(video, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.get(video, probe); err != nil || calls != 2 {
		t.Fatalf("changed file: %v, %d probes (want 2)", err, calls)
	}
}

func TestSynologyProbeCacheRemembersFailures(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.mp4")
	if err := os.WriteFile(broken, []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	probe := func() (*ffmpeg.FileProbeInfo, error) {
		calls++
		return nil, errors.New("invalid data")
	}
	c := &synologyProbeCache{}
	if _, err := c.get(broken, probe); err == nil {
		t.Fatal("first failure must be reported")
	}
	info, err := c.get(broken, probe)
	if err != nil || info == nil || info.Duration != 0 || calls != 1 {
		t.Fatalf("second get = %+v, %v after %d probes; want empty info without probing again", info, err, calls)
	}
}
