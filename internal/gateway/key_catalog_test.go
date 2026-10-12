package gateway

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// All list formats use the same permissions through the real server stack.
// Switching keys, changing their lists, or removing an account takes effect
// on the next request, even while the handler is kept alive.
func TestKeyCatalogPermissionsStayCurrent(t *testing.T) {
	fresh(t)
	p := provider.Provider{ID: "relay", Name: "Relay", Chat: "http://relay.invalid/v1", Key: "first", Models: []string{"m1", "m2"},
		Keys:          []provider.KeyAccount{{Key: "first"}, {Key: "second"}},
		AccountModels: map[string][]string{provider.KeyID("first"): {"m1"}, provider.KeyID("second"): {"m2"}}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{Name: "Solo", Members: []string{"relay/m1"}}); err != nil {
		t.Fatal(err)
	}
	keys, secrets := newCaller(t, "First", "Second")
	for i, ref := range []string{"first", "second"} {
		if _, err := access.Update("models-key", access.Change{Key: keys[i].ID, Models: []string{"RELAY/*", "group/*"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := access.Update("accounts-key", access.Change{Key: keys[i].ID, Accounts: []string{"relay/" + provider.KeyID(ref)}}); err != nil {
			t.Fatal(err)
		}
	}
	h := lanGuard(New().Handler())
	list := func(path, secret string) []string {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "192.168.1.9:5000"
		r.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var body struct {
			Data   []struct{ ID string }
			Models []struct{ Name, Slug string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, m := range body.Data {
			ids = append(ids, m.ID)
		}
		for _, m := range body.Models {
			if m.Slug != "" {
				ids = append(ids, m.Slug)
			} else {
				ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
			}
		}
		slices.Sort(ids)
		return ids
	}
	check := func(key int, want []string) {
		t.Helper()
		for _, path := range []string{"/v1/models", "/v1/codex/models", "/v1beta/models"} {
			if got := list(path, secrets[key]); !slices.Equal(got, want) {
				t.Fatalf("key %d, %s: %v, want %v", key, path, got, want)
			}
		}
	}
	check(0, []string{"group/solo", "relay/m1"})
	check(1, []string{"relay/m2"})
	check(0, []string{"group/solo", "relay/m1"})
	if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{"relay/m2"}}); err != nil {
		t.Fatal(err)
	}
	check(0, nil) // its allowed account still serves m1 alone
	if _, err := access.Update("accounts-key", access.Change{Key: keys[0].ID, Accounts: []string{"relay/" + provider.KeyID("second")}}); err != nil {
		t.Fatal(err)
	}
	check(0, []string{"relay/m2"})
	p.Keys = p.Keys[:1] // a kept restriction on an account now gone serves none
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	check(0, nil)
	check(1, nil)
}

// Two providers expose 1,000 models and 500 derived groups. Filter-only
// isolates restricted-key overhead; the HTTP case includes authentication,
// catalog construction and JSON for ten different keys on one handler.
func BenchmarkKeyCatalog(b *testing.B) {
	b.Setenv("XDG_CONFIG_HOME", b.TempDir())
	b.Setenv("XDG_CACHE_HOME", b.TempDir())
	s := settings.Load()
	s.LAN = true
	if err := settings.Save(s); err != nil {
		b.Fatal(err)
	}
	models := make([]string, 500)
	for i := range models {
		models[i] = fmt.Sprintf("model-%03d", i)
	}
	for _, id := range []string{"a", "b"} {
		if err := provider.Save(provider.Provider{ID: id, Name: id, Chat: "http://" + id + ".invalid/v1", Key: "key", Models: models}); err != nil {
			b.Fatal(err)
		}
	}
	es := provider.Catalog()
	patterns := make([]string, 8)
	for i := range patterns {
		patterns[i] = "a/" + models[i]
	}
	for name, who := range map[string]access.Identity{
		"unrestricted": {}, "exact": {Models: patterns}, "wildcard": {Models: []string{"a/*"}},
		"accounts": {Models: patterns, Accounts: []string{"a/" + provider.KeyID("key")}},
	} {
		b.Run("filter/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				r := httptest.NewRequest("GET", "/v1/models", nil)
				r = r.WithContext(access.WithIdentity(r.Context(), who))
				keyAllowed(r, es)
			}
		})
	}
	var secrets []string
	for i := range 10 {
		secret, err := access.Update("add-key", access.Change{Name: fmt.Sprintf("Key %d", i)})
		if err != nil {
			b.Fatal(err)
		}
		who, ok := access.Authenticate(secret)
		if !ok {
			b.Fatal("new key did not authenticate")
		}
		if _, err := access.Update("models-key", access.Change{Key: who.KeyID, Models: []string{"a/" + models[i]}}); err != nil {
			b.Fatal(err)
		}
		secrets = append(secrets, secret)
	}
	h := lanGuard(New().Handler())
	b.Run("http/ten-keys", func(b *testing.B) {
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			r := httptest.NewRequest("GET", "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer "+secrets[i%len(secrets)])
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "a/"+models[i%len(secrets)]) {
				b.Fatalf("%d %s", w.Code, w.Body.String())
			}
			i++
		}
	})
}
