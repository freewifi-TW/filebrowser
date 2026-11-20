//go:build linux

package synothumb

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Events we care about: a file was completely written (IN_CLOSE_WRITE) or moved
// into place (IN_MOVED_TO), and directories appearing or leaving.
const watchMask = syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO | syscall.IN_MOVED_FROM |
	syscall.IN_CREATE | syscall.IN_ONLYDIR

// settleDelay is how long a file must stay quiet after its last event before it is processed.
const settleDelay = 3 * time.Second

type watcher struct {
	g    *Generator
	file *os.File // inotify descriptor, non-blocking so Close unblocks Read
	fd   int      // same descriptor; os.File.Fd() is avoided because it may switch it to blocking mode
	out  chan string

	mu          sync.Mutex
	dirs        map[int32]string     // watch descriptor -> directory
	pending     map[string]time.Time // video path -> time it becomes due
	limitWarned bool
}

// startWatcher watches every directory below the generator's roots with inotify.
// It returns a channel of video paths that were just written; an empty string
// means "events were lost, rescan". It returns nil when inotify is unavailable.
func startWatcher(ctx context.Context, g *Generator) <-chan string {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		g.cfg.Info("synology thumbnails: live watching unavailable (%v), relying on periodic scans", err)
		return nil
	}
	w := &watcher{
		g:       g,
		fd:      fd,
		file:    os.NewFile(uintptr(fd), "inotify"),
		out:     make(chan string, 4096),
		dirs:    map[int32]string{},
		pending: map[string]time.Time{},
	}
	go func() {
		<-ctx.Done()
		w.file.Close()
	}()
	go w.flushLoop(ctx)
	go func() {
		for _, root := range g.cfg.Roots {
			w.addTree(root, false)
		}
		w.mu.Lock()
		n := len(w.dirs)
		w.mu.Unlock()
		g.cfg.Info("synology thumbnails: watching %d folders for new videos", n)
		w.readLoop()
	}()
	return w.out
}

// addTree adds watches for dir and everything below it. With queueVideos it also
// queues the videos already inside: they may have been written before the watch existed.
func (w *watcher) addTree(dir string, queueVideos bool) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != dir && skipDir(d.Name()) {
				return fs.SkipDir
			}
			if !w.addWatch(path) {
				return fs.SkipAll
			}
			return nil
		}
		if queueVideos && d.Type().IsRegular() {
			w.queue(path)
		}
		return nil
	})
}

// addWatch returns false when the system-wide watch limit is exhausted.
func (w *watcher) addWatch(dir string) bool {
	wd, err := syscall.InotifyAddWatch(w.fd, dir, watchMask)
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			w.mu.Lock()
			warned := w.limitWarned
			w.limitWarned = true
			w.mu.Unlock()
			if !warned {
				w.g.cfg.Info("synology thumbnails: inotify watch limit reached (fs.inotify.max_user_watches); " +
					"folders beyond it are only covered by the periodic scan")
			}
			return false
		}
		return true // unreadable or vanished directory: skip just this one
	}
	w.mu.Lock()
	w.dirs[int32(wd)] = dir
	w.mu.Unlock()
	return true
}

// removeTree drops the watches of dir and its subdirectories (dir was renamed or moved away).
func (w *watcher) removeTree(dir string) {
	prefix := dir + string(filepath.Separator)
	w.mu.Lock()
	defer w.mu.Unlock()
	for wd, path := range w.dirs {
		if path == dir || strings.HasPrefix(path, prefix) {
			_, _ = syscall.InotifyRmWatch(w.fd, uint32(wd))
			delete(w.dirs, wd)
		}
	}
}

func (w *watcher) queue(path string) {
	name := filepath.Base(path)
	if strings.HasPrefix(name, ".") || !w.g.isVideo(name) {
		return
	}
	w.mu.Lock()
	w.pending[path] = time.Now().Add(settleDelay)
	w.mu.Unlock()
}

// flushLoop hands over files that have been quiet for settleDelay.
func (w *watcher) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			var due []string
			w.mu.Lock()
			for path, at := range w.pending {
				if !now.Before(at) {
					due = append(due, path)
					delete(w.pending, path)
				}
			}
			w.mu.Unlock()
			for _, path := range due {
				w.send(path)
			}
		}
	}
}

// send never blocks: when the generator is far behind, the periodic scan catches up.
func (w *watcher) send(path string) {
	select {
	case w.out <- path:
	default:
	}
}

func (w *watcher) readLoop() {
	buf := make([]byte, 64*1024)
	for {
		n, err := w.file.Read(buf)
		if err != nil {
			return // closed on shutdown
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameLen := int(ev.Len)
			start := off + syscall.SizeofInotifyEvent
			if start+nameLen > n {
				break
			}
			name := strings.TrimRight(string(buf[start:start+nameLen]), "\x00")
			off = start + nameLen
			w.handle(ev.Wd, ev.Mask, name)
		}
	}
}

func (w *watcher) handle(wd int32, mask uint32, name string) {
	if mask&syscall.IN_Q_OVERFLOW != 0 {
		w.send("") // events were dropped by the kernel: ask for a full scan
		return
	}
	if mask&syscall.IN_IGNORED != 0 {
		w.mu.Lock()
		delete(w.dirs, wd)
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	dir, ok := w.dirs[wd]
	w.mu.Unlock()
	if !ok || name == "" {
		return
	}
	path := filepath.Join(dir, name)

	if mask&syscall.IN_ISDIR != 0 {
		switch {
		case mask&syscall.IN_MOVED_FROM != 0:
			w.removeTree(path)
		case mask&(syscall.IN_CREATE|syscall.IN_MOVED_TO) != 0:
			if !skipDir(name) {
				w.addTree(path, true)
			}
		}
		return
	}
	if mask&(syscall.IN_CLOSE_WRITE|syscall.IN_MOVED_TO) != 0 {
		w.queue(path)
	}
}
