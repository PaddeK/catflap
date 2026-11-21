// Package update asks GitHub for the latest catflap release. The homelab
// checks daily (alerts), `catflap version` when it's run.
package update

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
)

const (
	releasesURL = "https://api.github.com/repos/PaddeK/catflap/releases/latest"
	// UpgradeDocURL explains how to upgrade.
	UpgradeDocURL = "https://github.com/PaddeK/catflap/blob/main/docs/using-catflap.md#upgrading"
)

// Release is a published catflap release.
type Release struct {
	Version string // without the leading "v"
	URL     string // release page
}

// Latest returns the latest release. CATFLAP_UPDATE_URL replaces the GitHub
// API URL (tests).
func Latest(ctx context.Context, timeout time.Duration) (Release, error) {
	url := releasesURL
	if u := os.Getenv("CATFLAP_UPDATE_URL"); u != "" {
		url = u
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "catflap/"+api.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	var r struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Draft  bool   `json:"draft"`
		Prerel bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("reading the release: %w", err)
	}
	v := strings.TrimPrefix(r.Tag, "v")
	if !IsRelease(v) || r.Draft || r.Prerel {
		return Release{}, errors.New("unexpected release " + strconv.Quote(r.Tag))
	}
	if !strings.HasPrefix(r.URL, "https://github.com/") {
		r.URL = "https://github.com/PaddeK/catflap/releases"
	}
	return Release{Version: v, URL: r.URL}, nil
}

// IsRelease reports whether v is a release version ("1.2.3" or "v1.2.3"),
// not a dev build.
func IsRelease(v string) bool {
	_, ok := parse(v)
	return ok
}

func parse(s string) ([3]int, bool) {
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

// Newer reports whether release version a is newer than b.
func Newer(a, b string) bool {
	va, ok1 := parse(a)
	vb, ok2 := parse(b)
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
