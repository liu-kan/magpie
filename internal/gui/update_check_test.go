package gui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/update"
)

func TestProxyCanReplaceAnUpdateBeforeItStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old := latestUpdate
		var calls atomic.Int32
		latestUpdate = func(context.Context, string) (*update.Release, error) {
			calls.Add(1)
			return &update.Release{Version: "9.9.9"}, nil
		}
		defer func() { latestUpdate = old }()
		u := &updater{}
		c := u.begin(false)
		u.proxyChanged()
		go u.run(c) // the replaced check was not yet scheduled
		synctest.Wait()
		if j := u.json(); j.State == "checking" || j.State == "error" || j.Latest != "9.9.9" || calls.Load() != 1 {
			t.Fatalf("a replaced queued check still ran: %+v, %d reads", j, calls.Load())
		}
	})
}

func TestReplacedUpdateCannotPublishALateAnswer(t *testing.T) {
	for _, answer := range []string{"success", "failure"} {
		t.Run(answer, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				oldVersion, oldLatest := Version, latestUpdate
				Version = "0.1.10"
				held := make(chan struct{})
				var release sync.Once
				var calls atomic.Int32
				var first context.Context
				latestUpdate = func(ctx context.Context, lang string) (*update.Release, error) {
					if calls.Add(1) == 1 {
						first = ctx
						<-held // deliberately answers despite cancellation
						if answer == "failure" {
							return nil, errors.New("old network failed")
						}
						return &update.Release{Version: "9.9.9"}, nil
					}
					return &update.Release{Version: Version}, nil
				}
				defer func() {
					release.Do(func() { close(held) })
					synctest.Wait()
					Version, latestUpdate = oldVersion, oldLatest
				}()
				// A late newer release would try to download (and fail for
				// its missing asset) if it were allowed to change this check.
				u := &updater{exe: "unused"}
				go u.check()
				synctest.Wait()
				if first == nil || u.json().State != "checking" {
					t.Fatal("the first check did not wait")
				}
				u.proxyChanged()
				synctest.Wait()
				if first.Err() != context.Canceled {
					t.Fatalf("old check was not canceled: %v", first.Err())
				}
				if j := u.json(); j.State != "latest" || j.Latest != Version || calls.Load() != 2 {
					t.Fatalf("new check: %+v, %d feed reads", j, calls.Load())
				}
				release.Do(func() { close(held) })
				synctest.Wait()
				if j := u.json(); j.State != "latest" || j.Latest != Version || j.Error != "" || j.Retry {
					t.Fatalf("old %s changed the new check: %+v", answer, j)
				}
			})
		})
	}
}

func TestProxyReplacementKeepsAnAutomaticUpdateBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldVersion, oldLatest := Version, latestUpdate
		Version = "0.1.10"
		var calls atomic.Int32
		latestUpdate = func(ctx context.Context, lang string) (*update.Release, error) {
			if calls.Add(1) == 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &update.Release{Version: "0.1.11"}, nil
		}
		defer func() { Version, latestUpdate = oldVersion, oldLatest }()
		u := &updater{exe: "unused", blocked: &update.Blocked{Version: "0.1.11"}}
		go u.checkByClock()
		synctest.Wait()
		u.proxyChanged()
		synctest.Wait()
		if j := u.json(); j.State != "blocked" || calls.Load() != 2 {
			t.Fatalf("replacement forgot it was automatic: %+v, %d reads", j, calls.Load())
		}
		// A click still tries that version: the fixture has no asset, so
		// reaching the download is reported as its existing download error.
		u.check()
		if j := u.json(); j.State != "error" || !j.Retry || !strings.Contains(j.Error, "has no") {
			t.Fatalf("manual check did not try the blocked version: %+v", j)
		}
	})
}

// No proxy save or TUN notification is involved: a check that never hears
// from the feed ends, and the same settings can be used to check again.
func TestWaitingUpdateEndsWithoutAProxyChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := settings.Save(settings.Settings{Proxy: "direct"}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var calls atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(update.Release{Version: "9.9.9"})
	}))
	t.Cleanup(feed.Close)
	t.Setenv("MAGPIE_UPDATE_FEED", feed.URL)
	u := &updater{}
	done := make(chan struct{})
	go func() { u.check(); close(done) }()
	t.Cleanup(func() { feed.CloseClientConnections(); <-done })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("check did not reach the feed")
	}
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		t.Fatal("update never ended without a proxy change")
	}
	if j := u.json(); j.State != "error" || !strings.Contains(j.Error, "deadline exceeded") {
		t.Fatalf("waiting check: %+v", j)
	}
	u.check()
	if j := u.json(); j.State == "error" || j.State == "checking" || j.Latest != "9.9.9" || calls.Load() != 2 {
		t.Fatalf("check again with the same proxy: %+v, %d reads", j, calls.Load())
	}
}

func TestProxyChangeLeavesAnUpdateDownloadRunning(t *testing.T) {
	body := []byte("new magpie")
	sum := sha256.Sum256(body)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var reads atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			close(entered)
			select {
			case <-release:
				w.Write(body)
			case <-r.Context().Done():
			}
			return
		}
		reads.Add(1)
		json.NewEncoder(w).Encode(update.Release{Version: "0.1.11", Assets: map[string]update.Asset{
			update.BinaryAsset(): {URL: "http://" + r.Host + "/asset", SHA256: hex.EncodeToString(sum[:])},
		}})
	}))
	t.Cleanup(feed.Close)
	t.Setenv("MAGPIE_UPDATE_FEED", feed.URL)
	old := Version
	Version = "0.1.10"
	t.Cleanup(func() { Version = old })
	exe, err := update.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(exe + ".new") })
	u := &updater{exe: exe}
	u.self, _ = os.Stat(exe)
	done := make(chan struct{})
	go func() { u.check(); close(done) }()
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		feed.CloseClientConnections()
		<-done
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	u.proxyChanged()
	if j := u.json(); j.State != "downloading" || reads.Load() != 1 {
		t.Fatalf("proxy change replaced the download: %+v, %d reads", j, reads.Load())
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not finish")
	}
	if j := u.json(); j.State != "ready" || reads.Load() != 1 {
		t.Fatalf("download after proxy change: %+v, %d reads", j, reads.Load())
	}
}
