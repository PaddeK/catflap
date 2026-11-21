package homelab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/notify"
)

// The update check: once a day the homelab asks GitHub for the latest
// catflap release and sends one alert per new version. Turned off with
// "check_updates": false in the settings file. Builds that aren't releases
// (dev builds) never check.

const (
	releasesURL   = "https://api.github.com/repos/PaddeK/catflap/releases/latest"
	UpgradeDocURL = "https://github.com/PaddeK/catflap/blob/main/docs/using-catflap.md#upgrading"
	updateEvery   = 24 * time.Hour
	updateDelay   = 2 * time.Minute // after the start, so a restart loop doesn't hammer GitHub
)

func (d *Daemon) updateLoop(ctx context.Context) {
	if !d.cfg.checkUpdates() {
		return
	}
	if _, ok := parseVersion(api.Version); !ok {
		return // a dev build
	}
	url := releasesURL
	if u := os.Getenv("CATFLAP_UPDATE_URL"); u != "" { // tests
		url = u
	}
	wait := updateDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = updateEvery
		rel, err := latestRelease(ctx, url)
		if err != nil {
			if ctx.Err() == nil {
				d.logf("update check: %v", err)
			}
			continue
		}
		d.updateFound(rel)
	}
}

// updateFound records the latest release and alerts once per newer version.
func (d *Daemon) updateFound(rel release) {
	if !newerVersion(rel.Version, api.Version) {
		d.mu.Lock()
		d.latest = ""
		d.mu.Unlock()
		return
	}
	d.mu.Lock()
	d.latest = rel.Version
	first := d.st.UpdateNotified != rel.Version
	if first {
		d.st.UpdateNotified = rel.Version
		d.saveLocked()
	}
	d.mu.Unlock()
	if !first {
		return
	}
	d.logf("update check: catflap %s is available (running %s)", rel.Version, api.Version)
	d.alert(notify.Message{Event: "update.available", Title: "catflap " + rel.Version + " is available",
		Body: fmt.Sprintf("This homelab runs catflap %s. What's new: %s\nHow to upgrade: %s", api.Version, rel.URL, UpgradeDocURL)}, "")
}

type release struct {
	Version string // without the leading "v"
	URL     string // release page
}

func latestRelease(ctx context.Context, url string) (release, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "catflap/"+api.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	var r struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Draft  bool   `json:"draft"`
		Prerel bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&r); err != nil {
		return release{}, fmt.Errorf("reading the release: %w", err)
	}
	v := strings.TrimPrefix(r.Tag, "v")
	if _, ok := parseVersion(v); !ok || r.Draft || r.Prerel {
		return release{}, errors.New("unexpected release " + strconv.Quote(r.Tag))
	}
	if !strings.HasPrefix(r.URL, "https://github.com/") {
		r.URL = "https://github.com/PaddeK/catflap/releases"
	}
	return release{Version: v, URL: r.URL}, nil
}

// parseVersion parses a release version "1.2.3" (or "v1.2.3").
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// newerVersion reports whether release version a is newer than b.
func newerVersion(a, b string) bool {
	va, ok1 := parseVersion(a)
	vb, ok2 := parseVersion(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range va {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}
