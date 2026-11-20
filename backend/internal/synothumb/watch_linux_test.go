//go:build linux

package synothumb

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWatcherGeneratesForNewVideos(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A long interval and MinAge: anything generated here can only come from the watcher.
	g, err := New(Config{FFmpegPath: "ffmpeg", Roots: []string{root}, Interval: time.Hour, MinAge: time.Hour, Watch: true})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	g.generate = func(_ context.Context, videoPath string, _ time.Time) error {
		mu.Lock()
		seen[videoPath] = true
		mu.Unlock()
		for _, th := range g.cfg.Thumbs {
			if err := os.MkdirAll(ThumbDir(videoPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(ThumbDir(videoPath), th.Name), []byte("x"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	time.Sleep(time.Second) // initial scan + watches in place

	write := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inExisting := filepath.Join(root, "existing", "upload.mp4")
	write(inExisting)
	write(filepath.Join(root, "existing", "notes.txt"))

	// A folder created after start-up is watched too.
	newDir := filepath.Join(root, "new folder")
	if err := os.Mkdir(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	inNewDir := filepath.Join(newDir, "clip.mkv")
	write(inNewDir)

	// A file moved into place (how most uploaders finish a transfer).
	tmp := filepath.Join(root, "existing", ".upload.tmp")
	write(tmp)
	moved := filepath.Join(root, "existing", "moved.mov")
	if err := os.Rename(tmp, moved); err != nil {
		t.Fatal(err)
	}

	want := []string{inExisting, inNewDir, moved}
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		missing := 0
		for _, p := range want {
			if !seen[p] {
				missing++
			}
		}
		total := len(seen)
		mu.Unlock()
		if missing == 0 {
			if total != len(want) {
				t.Fatalf("generated for unexpected files: %v", seen)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher did not report all videos, got %v", seen)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
