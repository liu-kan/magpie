package gui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/update"
)

// An update check that failed going direct is tried again as soon as a
// proxy is set on the Settings page, through it (#294), rather than the
// error staying up till the next check six hours on. The answer to the
// save already finds it checking: the page reads /api/update straight
// after, and an error still up then was drawn with nothing asking again.
// Marked inside the check's goroutine, it was still up in about a third of
// runs here.
func TestSettingsProxyRechecksAFailedUpdate(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	// A TUN may give .invalid a Fake-IP and accept its connection forever.
	// Force the direct lookup to fail here, without asking the real DNS.
	oldResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "feed.magpie.invalid", IsNotFound: true}
	}}
	defer func() { net.DefaultResolver = oldResolver }()
	var hits atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "feed.magpie.invalid" {
			hits.Add(1)
		}
		json.NewEncoder(w).Encode(update.Release{Version: "9.9.9"})
	}))
	defer px.Close()
	// direct, the feed is nowhere to be found
	if err := settings.Save(settings.Settings{Proxy: "direct"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_UPDATE_FEED", "http://feed.magpie.invalid/api/latest")
	old := updates
	updates = &updater{}
	defer func() { updates = old }()
	updates.check()
	if j := updates.json(); j.State != "error" {
		t.Fatalf("direct: %+v", j)
	}

	rec := httptest.NewRecorder()
	mux := Handler(nil, nil)
	procs := runtime.GOMAXPROCS(1)
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"proxy":"`+px.URL+`"}`)))
	j := updates.json()
	runtime.GOMAXPROCS(procs)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if j.State == "error" {
		t.Fatalf("the save answered with the old error up: %+v", j)
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

// A proxy set while the feed is still waiting replaces the old check,
// instead of leaving it to finish through the old network path.
func TestSettingsProxyReplacesAWaitingUpdate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := settings.Save(settings.Settings{Proxy: "direct"}); err != nil {
		t.Fatal(err)
	}
	entered, stopped := make(chan struct{}), make(chan struct{})
	var hits atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(entered)
			<-r.Context().Done()
			close(stopped)
			return
		}
		json.NewEncoder(w).Encode(update.Release{Version: "9.9.9"})
	}))
	t.Cleanup(feed.Close)
	t.Setenv("MAGPIE_UPDATE_FEED", feed.URL)
	old := updates
	u := &updater{}
	updates = u
	t.Cleanup(func() { updates = old })
	done := make(chan struct{})
	go func() { u.check(); close(done) }()
	// Closing the server first also releases the old check on a failure.
	t.Cleanup(func() { feed.CloseClientConnections(); <-done })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the direct check never reached the feed")
	}
	rec := httptest.NewRecorder()
	Handler(nil, nil).ServeHTTP(rec, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"proxy":"http://127.0.0.1:7890"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		feed.CloseClientConnections()
		<-done
		t.Fatal("saving the proxy did not cancel the waiting update")
	}
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for u.json().State == "checking" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if j := u.json(); j.State == "checking" || j.State == "error" || j.Latest != "9.9.9" || hits.Load() != 2 {
		t.Fatalf("the replacement check: %+v, %d requests", j, hits.Load())
	}
}
