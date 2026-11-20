//go:build !linux

package synothumb

import "context"

// startWatcher: live watching uses inotify and is only implemented on Linux.
func startWatcher(context.Context, *Generator) <-chan string {
	return nil
}
