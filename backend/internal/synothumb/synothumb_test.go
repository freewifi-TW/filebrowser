package synothumb

import (
	"context"
	"errors"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "image/jpeg"
)

func writeFile(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// newStubGenerator returns a generator whose "ffmpeg" just writes placeholder thumbnails.
func newStubGenerator(t *testing.T, root string, fail map[string]bool) (*Generator, *[]string) {
	t.Helper()
	g, err := New(Config{FFmpegPath: "ffmpeg", Roots: []string{root}, MinAge: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	g.generate = func(_ context.Context, videoPath string, _ time.Time) error {
		calls = append(calls, filepath.Base(videoPath))
		if fail[filepath.Base(videoPath)] {
			return errors.New("boom")
		}
		for _, th := range g.cfg.Thumbs {
			writeFile(t, filepath.Join(ThumbDir(videoPath), th.Name), time.Time{})
		}
		return nil
	}
	return g, &calls
}

func TestThumbDir(t *testing.T) {
	got := ThumbDir(filepath.Join("vol", "share", "clip.mp4"))
	want := filepath.Join("vol", "share", "@eaDir", "clip.mp4")
	if got != want {
		t.Fatalf("ThumbDir = %q, want %q", got, want)
	}
}

func TestSkipDir(t *testing.T) {
	for name, want := range map[string]bool{
		"@eaDir": true, "@tmp": true, "#recycle": true, "#snapshot": true, ".git": true,
		"Videos": false, "2024 trip": false,
	} {
		if got := skipDir(name); got != want {
			t.Errorf("skipDir(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestScanOnce(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-time.Hour)

	writeFile(t, filepath.Join(root, "a.mp4"), old)
	writeFile(t, filepath.Join(root, "sub", "b.MKV"), old)
	writeFile(t, filepath.Join(root, "sub", "broken.mov"), old)
	writeFile(t, filepath.Join(root, "fresh.mp4"), time.Now())                // still uploading
	writeFile(t, filepath.Join(root, "notes.txt"), old)                       // not a video
	writeFile(t, filepath.Join(root, ".hidden.mp4"), old)                     // hidden file
	writeFile(t, filepath.Join(root, "#recycle", "gone.mp4"), old)            // recycle bin
	writeFile(t, filepath.Join(root, "@eaDir", "a.mp4", "SYNOPHOTO_FILM.mp4"), old) // Synology metadata

	g, calls := newStubGenerator(t, root, map[string]bool{"broken.mov": true})

	stats := g.ScanOnce(context.Background())
	want := Stats{Videos: 4, Generated: 2, Skipped: 1, Failed: 1}
	if stats != want {
		t.Fatalf("first scan stats = %+v, want %+v (calls %v)", stats, want, *calls)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "@eaDir", "b.MKV", "SYNOPHOTO_THUMB_XL.jpg")); err != nil {
		t.Fatalf("thumbnail not written: %v", err)
	}

	// Second scan: nothing regenerated, the failed file is not retried.
	*calls = nil
	stats = g.ScanOnce(context.Background())
	want = Stats{Videos: 4, UpToDate: 2, Skipped: 2}
	if stats != want {
		t.Fatalf("second scan stats = %+v, want %+v", stats, want)
	}
	if len(*calls) != 0 {
		t.Fatalf("second scan regenerated %v", *calls)
	}

	// A video that changed after its thumbnail was made is regenerated,
	// and a previously failing file is retried once its mtime changes.
	future := time.Now().Add(-2 * time.Minute)
	thumbOld := time.Now().Add(-30 * time.Minute)
	marker := filepath.Join(root, "@eaDir", "a.mp4", "SYNOPHOTO_THUMB_XL.jpg")
	if err := os.Chtimes(marker, thumbOld, thumbOld); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "a.mp4"), future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "sub", "broken.mov"), future, future); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	stats = g.ScanOnce(context.Background())
	want = Stats{Videos: 4, Generated: 1, UpToDate: 1, Skipped: 1, Failed: 1}
	if stats != want {
		t.Fatalf("third scan stats = %+v, want %+v (calls %v)", stats, want, *calls)
	}
}

func TestScanStopsOnCancel(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.mp4"), time.Now().Add(-time.Hour))
	g, calls := newStubGenerator(t, root, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.Run(ctx)
	if len(*calls) != 0 {
		t.Fatalf("generated after cancel: %v", *calls)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{Roots: []string{"x"}}); err == nil {
		t.Error("expected error without ffmpeg path")
	}
	if _, err := New(Config{FFmpegPath: "ffmpeg"}); err == nil {
		t.Error("expected error without roots")
	}
}

// TestGenerateWithRealFFmpeg runs only when ffmpeg and ffprobe are on PATH.
func TestGenerateWithRealFFmpeg(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ffprobePath, _ := exec.LookPath("ffprobe")

	root := t.TempDir()
	video := filepath.Join(root, "clip with space.mp4")
	// 1920x1080 test pattern; mpeg4 is available in every ffmpeg build that can encode at all.
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=4:size=1920x1080:rate=10", "-c:v", "mpeg4", video).CombinedOutput()
	if err != nil {
		t.Skipf("this ffmpeg build cannot create the sample video: %v: %s", err, out)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(video, past, past); err != nil {
		t.Fatal(err)
	}

	g, err := New(Config{FFmpegPath: ffmpegPath, FFprobePath: ffprobePath, Roots: []string{root}, MinAge: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	stats := g.ScanOnce(context.Background())
	if stats.Generated != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	for _, th := range DefaultThumbs {
		f, err := os.Open(filepath.Join(ThumbDir(video), th.Name))
		if err != nil {
			t.Fatal(err)
		}
		cfg, format, err := image.DecodeConfig(f)
		f.Close()
		if err != nil || format != "jpeg" {
			t.Fatalf("%s: not a JPEG (%v, %q)", th.Name, err, format)
		}
		// 16:9 source: must fit the box, touch it on at least one side, and stay landscape.
		fits := cfg.Width <= th.Width && cfg.Height <= th.Height
		touches := th.Width-cfg.Width <= 2 || th.Height-cfg.Height <= 2
		if !fits || !touches || cfg.Height >= cfg.Width {
			t.Errorf("%s: got %dx%d for a %dx%d box", th.Name, cfg.Width, cfg.Height, th.Width, th.Height)
		}
	}
	entries, _ := os.ReadDir(ThumbDir(video))
	if len(entries) != len(DefaultThumbs) {
		t.Errorf("leftover temp files in %s: %v", ThumbDir(video), entries)
	}
	// A DSM failure marker is removed, and an existing current thumbnail is left untouched.
	fail := filepath.Join(ThumbDir(video), "SYNOPHOTO_THUMB_M.fail")
	keep := filepath.Join(ThumbDir(video), "SYNOFILE_THUMB_XL.jpg")
	if err := os.WriteFile(fail, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("made by DSM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ThumbDir(video), "SYNOFILE_THUMB_S.jpg")); err != nil {
		t.Fatal(err)
	}
	if st := g.ScanOnce(context.Background()); st.Generated != 1 {
		t.Fatalf("repair scan stats = %+v", st)
	}
	if _, err := os.Stat(fail); err == nil {
		t.Error(".fail marker was not removed")
	}
	if b, _ := os.ReadFile(keep); string(b) != "made by DSM" {
		t.Error("an existing up-to-date thumbnail was overwritten")
	}
	if again := g.ScanOnce(context.Background()); again.UpToDate != 1 || again.Generated != 0 {
		t.Fatalf("second scan stats = %+v", again)
	}
}
