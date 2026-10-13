package gui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/update"
)

// A source build can read releases but only downloads the exact version
// the reader agreed would replace it. The real asset is hash checked.
func TestSourceUpdateNeedsConfirmation(t *testing.T) {
	for _, version := range []string{"dev", "v0.1.10-2-g1234567", "0.1.10-dirty"} {
		t.Run(version, func(t *testing.T) {
			oldVersion, oldUpdates := Version, updates
			Version = version
			defer func() { Version, updates = oldVersion, oldUpdates }()
			var downloads atomic.Int32
			var released atomic.Value
			released.Store("0.1.1234")
			body := []byte("verified release fixture")
			sum := sha256.Sum256(body)
			feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/asset" {
					downloads.Add(1)
					w.Write(body)
					return
				}
				json.NewEncoder(w).Encode(update.Release{Version: released.Load().(string), Assets: map[string]update.Asset{
					update.BinaryAsset(): {URL: "http://" + r.Host + "/asset", SHA256: hex.EncodeToString(sum[:])},
				}})
			}))
			defer feed.Close()
			t.Setenv("MAGPIE_UPDATE_FEED", feed.URL)
			exe, err := update.Executable()
			if err != nil {
				t.Fatal(err)
			}
			u := &updater{exe: exe}
			u.self, _ = os.Stat(exe)
			updates = u
			defer os.Remove(exe + ".new")
			defer func() {
				u.mu.Lock()
				if u.checkIn != nil {
					u.checkIn.cancel()
				}
				u.mu.Unlock()
				waitSourceCheck(t, u)
			}()
			call := func(body string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				Handler(nil, nil).ServeHTTP(rec, httptest.NewRequest("POST", "/api/update/install", strings.NewReader(body)))
				return rec
			}
			for _, check := range []func(){u.check, u.checkByClock} {
				check()
				if j := u.json(); j.State != "source" || j.Latest != "0.1.1234" || downloads.Load() != 0 {
					t.Fatalf("unconfirmed check: %+v, %d downloads", j, downloads.Load())
				}
			}
			for _, body := range []string{`{}`, `{"replaceSource":"0.1.1233"}`} {
				call(body)
				if j := u.json(); j.State != "source" || downloads.Load() != 0 {
					t.Fatalf("unconfirmed install %s: %+v, %d downloads", body, j, downloads.Load())
				}
			}
			if rec := call(`{"replaceSource":"0.1.1234"}`); rec.Code != http.StatusOK {
				t.Fatalf("confirmation: %d %s", rec.Code, rec.Body.String())
			}
			waitSourceCheck(t, u)
			if j := u.json(); j.State != "ready" || j.Latest != "0.1.1234" || downloads.Load() != 1 {
				t.Fatalf("confirmed install: %+v, %d downloads", j, downloads.Load())
			}
			got, err := os.ReadFile(u.staged)
			if err != nil || string(got) != string(body) {
				t.Fatalf("staged asset: %q, %v", got, err)
			}
			// Restart's freshness check retains the confirmed, staged version.
			u.check()
			released.Store("0.1.1235")
			u.checkByClock()
			u.check()
			if j := u.json(); j.State != "ready" || j.Latest != "0.1.1234" || downloads.Load() != 1 {
				t.Fatalf("a different release wasn't confirmed: %+v, %d downloads", j, downloads.Load())
			}
		})
	}
}

func waitSourceCheck(t *testing.T, u *updater) {
	t.Helper()
	// This bounds test cleanup, not the product's network timeout. All
	// requests stay on a local server; allow a loaded race-test runner room.
	deadline := time.Now().Add(30 * time.Second)
	for {
		u.mu.Lock()
		done := u.checkIn == nil
		u.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("source update didn't finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
