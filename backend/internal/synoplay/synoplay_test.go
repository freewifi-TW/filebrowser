package synoplay

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubRunner pretends to be ffmpeg/ffprobe: it writes the output file (last argument).
type stubRunner struct {
	mu        sync.Mutex
	calls     [][]string
	probeOut  string
	failCopy  bool          // fail any invocation that uses stream copy for the video
	failAll   bool          // fail every ffmpeg invocation
	encodeFor time.Duration // how long an encode takes
	writeSize int
}

func (s *stubRunner) run(_ context.Context, bin string, args ...string) ([]byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string{bin}, args...))
	s.mu.Unlock()
	if bin == "ffprobe" {
		return []byte(s.probeOut), nil
	}
	joined := strings.Join(args, " ")
	if s.failAll || (s.failCopy && strings.Contains(joined, "-c:v copy")) {
		return []byte("boom"), errors.New("exit status 1")
	}
	out := args[len(args)-1]
	size := s.writeSize
	if size == 0 {
		size = minPartialBytes + 1
	}
	if err := os.WriteFile(out, make([]byte, size), 0o644); err != nil {
		return nil, err
	}
	if strings.Contains(joined, "frag_keyframe") {
		time.Sleep(s.encodeFor)
	}
	return nil, nil
}

func (s *stubRunner) encodes() (n int, last string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if j := strings.Join(c, " "); strings.Contains(j, "frag_keyframe") {
			n++
			last = j
		}
	}
	return n, last
}

func newTestManager(t *testing.T, stub *stubRunner, mutate func(*Config)) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "home movie.mpg")
	if err := os.WriteFile(src, []byte("mpeg data"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{FFmpegPath: "ffmpeg", FFprobePath: "ffprobe", CacheDir: filepath.Join(dir, "cache"), FirstWait: 5 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.run = stub.run
	return m, src
}

func TestNeedsConversion(t *testing.T) {
	m, _ := newTestManager(t, &stubRunner{}, nil)
	for name, want := range map[string]bool{
		"a.mpg": true, "a.MPEG": true, "a.avi": true, "a.wmv": true, "a.vob": true,
		"a.mp4": false, "a.webm": false, "a.mkv": false, "a.mov": false, "noext": false,
	} {
		if got := m.NeedsConversion(name); got != want {
			t.Errorf("NeedsConversion(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPlayableConvertsOnceAndCaches(t *testing.T) {
	stub := &stubRunner{probeOut: "mpeg2video,video\nmp2,audio\n"}
	m, src := newTestManager(t, stub, nil)

	path, ok := m.Playable(context.Background(), src)
	if !ok || !strings.HasSuffix(path, ".mp4") || strings.Contains(path, "partial") {
		t.Fatalf("Playable = %q, %v", path, ok)
	}
	n, last := stub.encodes()
	if n != 1 || !strings.Contains(last, "-c:v libx264") || !strings.Contains(last, "-c:a aac") {
		t.Fatalf("unexpected encode calls (%d): %s", n, last)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(m.cfg.CacheDir, "*partial*")); len(leftovers) != 0 {
		t.Fatalf("partial file left behind: %v", leftovers)
	}

	again, ok := m.Playable(context.Background(), src)
	if !ok || again != path {
		t.Fatalf("second Playable = %q, %v", again, ok)
	}
	if n, _ := stub.encodes(); n != 1 {
		t.Fatalf("converted again: %d encodes", n)
	}

	// A changed source gets its own conversion.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	changed, ok := m.Playable(context.Background(), src)
	if !ok || changed == path {
		t.Fatalf("changed source served the stale conversion: %q", changed)
	}
}

func TestStreamCopyWhenAlreadyH264(t *testing.T) {
	stub := &stubRunner{probeOut: "h264,video\naac,audio\n"}
	m, src := newTestManager(t, stub, nil)
	if _, ok := m.Playable(context.Background(), src); !ok {
		t.Fatal("not playable")
	}
	if n, last := stub.encodes(); n != 1 || !strings.Contains(last, "-c:v copy") || !strings.Contains(last, "-c:a copy") {
		t.Fatalf("expected one stream-copy call, got %d: %s", n, last)
	}
}

func TestFallsBackToReencodeWhenCopyFails(t *testing.T) {
	stub := &stubRunner{probeOut: "h264,video\naac,audio\n", failCopy: true}
	m, src := newTestManager(t, stub, nil)
	if _, ok := m.Playable(context.Background(), src); !ok {
		t.Fatal("not playable after fallback")
	}
	if n, last := stub.encodes(); n != 2 || !strings.Contains(last, "-c:v libx264") {
		t.Fatalf("expected copy attempt then re-encode, got %d: %s", n, last)
	}
}

func TestFailureIsNotRetried(t *testing.T) {
	stub := &stubRunner{failAll: true}
	m, src := newTestManager(t, stub, nil)
	if _, ok := m.Playable(context.Background(), src); ok {
		t.Fatal("failed conversion reported as playable")
	}
	before, _ := stub.encodes()
	if _, ok := m.Playable(context.Background(), src); ok {
		t.Fatal("failed conversion reported as playable")
	}
	if after, _ := stub.encodes(); after != before {
		t.Fatalf("failed conversion was retried (%d -> %d)", before, after)
	}
}

func TestLongConversionHandsOutPartialFile(t *testing.T) {
	stub := &stubRunner{encodeFor: 1500 * time.Millisecond}
	m, src := newTestManager(t, stub, func(c *Config) { c.FirstWait = 300 * time.Millisecond })

	path, ok := m.Playable(context.Background(), src)
	if !ok || !strings.HasSuffix(path, partialSuffix) {
		t.Fatalf("expected the partial file while converting, got %q, %v", path, ok)
	}
	// A second viewer must not start a second conversion.
	if _, ok := m.Playable(context.Background(), src); !ok {
		t.Fatal("second request not playable")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		final, ok := m.Playable(context.Background(), src)
		if ok && !strings.HasSuffix(final, partialSuffix) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conversion never finished")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n, _ := stub.encodes(); n != 1 {
		t.Fatalf("expected a single conversion, got %d", n)
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	stub := &stubRunner{writeSize: 1000}
	m, src := newTestManager(t, stub, func(c *Config) { c.MaxCacheBytes = 2500 })

	old := filepath.Join(m.cfg.CacheDir, "old.mp4")
	recent := filepath.Join(m.cfg.CacheDir, "recent.mp4")
	for path, age := range map[string]time.Duration{old: 48 * time.Hour, recent: time.Hour} {
		if err := os.WriteFile(path, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	created, ok := m.Playable(context.Background(), src)
	if !ok {
		t.Fatal("not playable")
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("oldest conversion was not evicted")
	}
	for _, keep := range []string{recent, created} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was evicted: %v", filepath.Base(keep), err)
		}
	}
}

func TestNewRemovesInterruptedConversions(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "abc"+partialSuffix)
	done := filepath.Join(dir, "def"+finalSuffix)
	for _, f := range []string{stale, done} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(Config{FFmpegPath: "ffmpeg", CacheDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("interrupted conversion was kept")
	}
	if _, err := os.Stat(done); err != nil {
		t.Error("finished conversion was removed")
	}
}

// TestConvertWithRealFFmpeg needs an ffmpeg with the mpeg2video, libx264 and aac
// encoders on PATH. It is skipped otherwise, unless SYNOLOGY_REQUIRE_FFMPEG=1 (CI).
func TestConvertWithRealFFmpeg(t *testing.T) {
	require := os.Getenv("SYNOLOGY_REQUIRE_FFMPEG") == "1"
	skip := func(format string, args ...any) {
		if require {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		skip("ffmpeg not on PATH")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		skip("ffprobe not on PATH")
	}
	encoders, _ := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	for _, enc := range []string{"libx264", "mpeg2video", " aac "} {
		if !strings.Contains(string(encoders), enc) {
			skip("this ffmpeg build lacks the %s encoder", strings.TrimSpace(enc))
		}
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "sample.mpg")
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=3:size=720x480:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "mpeg2video", "-c:a", "mp2", "-f", "mpeg", src).CombinedOutput()
	if err != nil {
		t.Fatalf("creating the MPEG-2 sample failed: %v: %s", err, out)
	}

	m, err := New(Config{FFmpegPath: ffmpegPath, FFprobePath: ffprobePath, CacheDir: filepath.Join(dir, "cache"), FirstWait: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	path, ok := m.Playable(context.Background(), src)
	if !ok || strings.HasSuffix(path, partialSuffix) {
		t.Fatalf("Playable = %q, %v", path, ok)
	}
	probe, err := exec.Command(ffprobePath, "-v", "error",
		"-show_entries", "stream=codec_name:format=format_name,duration", "-of", "default=nw=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe on the result failed: %v", err)
	}
	got := string(probe)
	for _, want := range []string{"codec_name=h264", "codec_name=aac", "mp4"} {
		if !strings.Contains(got, want) {
			t.Errorf("converted file lacks %q:\n%s", want, got)
		}
	}
	// A regular (non-fragmented, faststart) MP4 has its "moov" index before the media data.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	moov, mdat := strings.Index(string(data), "moov"), strings.Index(string(data), "mdat")
	if moov < 0 || mdat < 0 || moov > mdat {
		t.Errorf("result is not a faststart MP4 (moov at %d, mdat at %d)", moov, mdat)
	}
}
