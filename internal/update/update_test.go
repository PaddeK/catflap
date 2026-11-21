package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PaddeK/catflap/internal/api"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.6.0", "0.5.1", true},
		{"0.5.10", "0.5.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.5.1", "0.5.1", false},
		{"0.5.0", "0.5.1", false},
		{"v0.6.0", "0.5.1", true},
		{"0.6.0", "dev", false},
		{"0.6.0", "0.6.0-dev", false},
		{"0.6", "0.5.1", false},
		{"0.06.0", "0.5.1", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestLatest(t *testing.T) {
	body := `{"tag_name":"v0.6.0","html_url":"https://github.com/PaddeK/catflap/releases/tag/v0.6.0","draft":false,"prerelease":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "catflap/"+api.Version {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()
	t.Setenv("CATFLAP_UPDATE_URL", srv.URL)
	ctx := context.Background()
	rel, err := Latest(ctx, 5*time.Second)
	if err != nil || rel.Version != "0.6.0" || rel.URL != "https://github.com/PaddeK/catflap/releases/tag/v0.6.0" {
		t.Fatalf("got %+v, %v", rel, err)
	}
	body = `{"tag_name":"nightly","html_url":"x"}`
	if _, err := Latest(ctx, 5*time.Second); err == nil {
		t.Error("accepted a tag that isn't a version")
	}
	body = `{"tag_name":"v0.7.0","html_url":"https://evil.example/"}`
	if rel, err := Latest(ctx, 5*time.Second); err != nil || rel.URL != "https://github.com/PaddeK/catflap/releases" {
		t.Errorf("foreign release URL not replaced: %+v, %v", rel, err)
	}
}
