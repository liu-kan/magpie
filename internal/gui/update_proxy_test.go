package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/update"
)

// An update check that failed through a broken proxy is tried again as soon
// as the proxy is corrected on the Settings page (#294), rather than the
// error staying up till the next check six hours on.
func TestSettingsProxyRechecksAFailedUpdate(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	var hits atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "192.0.2.1" {
			hits.Add(1)
		}
		json.NewEncoder(w).Encode(update.Release{Version: "9.9.9"})
	}))
	defer px.Close()
	var failed atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failed.Add(1)
		http.Error(w, "proxy unavailable", http.StatusBadGateway)
	}))
	defer broken.Close()
	// Both requests go to local proxies. A reserved public IP needs no
	// DNS lookup and is never classified as a host on the user's LAN.
	if err := settings.Save(settings.Settings{Proxy: broken.URL}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_UPDATE_FEED", "http://192.0.2.1/api/latest")
	old := updates
	updates = &updater{}
	defer func() { updates = old }()
	updates.check()
	if j := updates.json(); j.State != "error" || failed.Load() != 1 {
		t.Fatalf("broken proxy: %+v, %d requests", j, failed.Load())
	}

	rec := httptest.NewRecorder()
	Handler(nil, nil).ServeHTTP(rec, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"proxy":"`+px.URL+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		j := updates.json()
		if j.State != "error" && j.State != "checking" {
			if j.Latest != "9.9.9" || hits.Load() != 1 {
				t.Fatalf("%+v, %d through the proxy", j, hits.Load())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not checked again through the proxy: %+v, %d through it", j, hits.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
