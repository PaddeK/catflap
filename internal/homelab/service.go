package homelab

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

// maxLogSize is when the service log starts over (the previous one is kept
// as homelab.log.1).
const maxLogSize = 5 << 20

// RunService runs the daemon as launchd (macOS) or the Task Scheduler
// (Windows) start it: logging to p.Log instead of a journal, and starting
// again after an error, as systemd's Restart=always would. It returns once
// ctx is done or the daemon was asked to shut down.
func RunService(ctx context.Context, p Paths) error {
	if f, err := openServiceLog(p.Log); err == nil {
		defer f.Close()
		log.SetOutput(f)
		debug.SetCrashOutput(f, debug.CrashOptions{}) // panics, too
	} else {
		log.Printf("log file: %v (logging to stderr)", err)
	}
	for {
		err := Run(ctx, p)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		log.Printf("homelab stopped: %v; starting again in 10s", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
		}
	}
}

func openServiceLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSize {
		os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}
