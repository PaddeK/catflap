package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PaddeK/catflap/internal/api"
)

func TestVersionUpdateCheck(t *testing.T) {
	latest := "v0.6.0"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"tag_name":"`+latest+`","html_url":"https://github.com/PaddeK/catflap/releases/tag/`+latest+`"}`)
	}))
	defer srv.Close()
	t.Setenv("CATFLAP_UPDATE_URL", srv.URL)
	oldV, oldA := version, api.Version
	t.Cleanup(func() { version, api.Version = oldV, oldA })

	out := func() string {
		var b strings.Builder
		if err := cmdVersion(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	version, api.Version = "0.5.1", "0.5.1"
	if s := out(); !strings.Contains(s, "Update available: catflap 0.6.0 (https://github.com/PaddeK/catflap/releases/tag/v0.6.0)") || !strings.Contains(s, "using-catflap.md#upgrading") {
		t.Errorf("older version:\n%s", s)
	}
	version, api.Version = "0.6.0", "0.6.0"
	if s := out(); !strings.Contains(s, "This is the latest release.") {
		t.Errorf("latest version:\n%s", s)
	}
	n := calls
	version, api.Version = "dev", "dev"
	if s := out(); calls != n || strings.Contains(s, "release") {
		t.Errorf("a dev build checked:\n%s", s)
	}
	version, api.Version = "0.5.1", "0.5.1"
	t.Setenv("CATFLAP_NO_UPDATE_CHECK", "1")
	if out(); calls != n {
		t.Error("CATFLAP_NO_UPDATE_CHECK ignored")
	}
	t.Setenv("CATFLAP_NO_UPDATE_CHECK", "")
	t.Setenv("CATFLAP_UPDATE_URL", "http://127.0.0.1:1")
	if s := out(); !strings.Contains(s, "Could not check for a newer version") {
		t.Errorf("unreachable:\n%s", s)
	}
}
