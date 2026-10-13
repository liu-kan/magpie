package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

// Real HTTP over a pipe keeps these tests independent of DNS, TUN and the
// runner's speed. synctest advances the clock while the connection waits.
func feedPipe(t *testing.T, serve func(net.Conn, *http.Request)) {
	t.Helper()
	t.Setenv("MAGPIE_UPDATE_FEED", "http://feed.magpie.invalid/api/latest")
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		c, s := net.Pipe()
		go func() {
			defer s.Close()
			r, err := http.ReadRequest(bufio.NewReader(s))
			if err == nil {
				defer r.Body.Close()
				serve(s, r)
			}
		}()
		return c, nil
	}}
	old := client
	client = &http.Client{Timeout: old.Timeout, Transport: tr}
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		client = old
	})
}

func TestLatestBoundsTheWholeFeed(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				feedPipe(t, func(c net.Conn, r *http.Request) {
					if phase == "body" {
						fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{\"version\":")
					}
					io.Copy(io.Discard, c) // accepted, but never answers completely
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				start := time.Now()
				_, err := Latest(ctx)
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 15*time.Second {
					t.Fatalf("%s waited %v, error %v; want a feed deadline after 15s", phase, time.Since(start), err)
				}
				if ctx.Err() != nil {
					t.Fatalf("the feed canceled its caller: %v", ctx.Err())
				}
			})
		})
	}
}

func TestLatestKeepsCallerCancellation(t *testing.T) {
	for _, early := range []string{"deadline", "cancel"} {
		t.Run(early, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				feedPipe(t, func(c net.Conn, r *http.Request) { io.Copy(io.Discard, c) })
				ctx, cancel := context.WithCancel(context.Background())
				want := context.Canceled
				if early == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
					want = context.DeadlineExceeded
				} else {
					timer := time.AfterFunc(2*time.Second, cancel)
					defer timer.Stop()
				}
				defer cancel()
				start := time.Now()
				_, err := Latest(ctx)
				if !errors.Is(err, want) || time.Since(start) != 2*time.Second {
					t.Fatalf("waited %v, error %v; want caller's %v after 2s", time.Since(start), err, want)
				}
			})
		})
	}
}

func TestFeedDeadlineLeavesTheDownloadItsBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := []byte("new magpie")
		sum := sha256.Sum256(body)
		feedPipe(t, func(c net.Conn, r *http.Request) {
			if r.URL.Path == "/asset" {
				time.Sleep(20 * time.Second)
				fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				return
			}
			b := `{"version":"9.9.9"}`
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(b), b)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := Latest(ctx); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		err := download(ctx, Asset{URL: "http://127.0.0.1/asset", SHA256: hex.EncodeToString(sum[:])}, filepath.Join(t.TempDir(), "magpie.new"))
		if err != nil || time.Since(start) != 20*time.Second {
			t.Fatalf("download after the feed: %v, took %v", err, time.Since(start))
		}
	})
}
