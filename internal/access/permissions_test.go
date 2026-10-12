package access

import (
	"context"
	"sync"
	"testing"
)

func TestCallerPermissionsMatchIdentity(t *testing.T) {
	for _, patterns := range [][]string{
		nil, {"openai/gpt-5", "anthropic/*", "relay/*-mini"},
		{"relay/s", "relay/σ", "relay/k"},
		{"relay/İ"}, {"relay/i"},
		{"relay/ſ", "relay/ς", "relay/K"}, {"relay/[x]+?", "group/*", "relay/*\n*"},
	} {
		who := Identity{Models: patterns}
		caller := Caller(WithIdentity(context.Background(), who))
		for _, id := range []string{"", "openai/gpt-5", "OpenAI/GPT-5", "openai/gpt-5-mini", "anthropic/vendor/x", "relay/m-mini", "relay/m", "relay/S", "relay/ſ", "relay/Σ", "relay/ς", "relay/k", "relay/K", "relay/i", "relay/İ", "relay/ı", "relay/[x]+?", "group/outer", "relay/a\nb"} {
			if got, want := caller.Allows(id), who.Allows(id); got != want {
				t.Errorf("patterns %q, id %q: %v, want %v", patterns, id, got, want)
			}
		}
		if got, want := caller.Allows("missing/m", "anthropic/x"), who.Allows("missing/m", "anthropic/x"); got != want {
			t.Errorf("patterns %q, multiple ids: %v, want %v", patterns, got, want)
		}
	}
	for _, accounts := range [][]string{nil, {" Relay/KEY-A ", "codex/Seat/One"}, {"relay/gone"}, {"invalid", "relay/"}} {
		who := Identity{Accounts: accounts}
		caller := Caller(WithIdentity(context.Background(), who))
		for _, pid := range []string{"relay", " RELAY ", "codex", "other"} {
			for _, ref := range []string{"", "key-a", " KEY-A ", "key-b", "seat/one", "gone"} {
				if got, want := caller.AllowsAccount(pid, ref), who.AllowsAccount(pid, ref); got != want {
					t.Errorf("accounts %q, %q/%q: %v, want %v", accounts, pid, ref, got, want)
				}
			}
		}
	}
}

// Request permissions cannot be rewritten through the slices that supplied
// them. A later request takes the new list rather than a key's old cache.
func TestCallerPermissionsAreASnapshot(t *testing.T) {
	who := Identity{Models: []string{"relay/old"}, Accounts: []string{"relay/first"}}
	ctx := WithIdentity(context.Background(), who)
	who.Models[0], who.Accounts[0] = "relay/new", "relay/second"
	before, after := Caller(ctx), Caller(WithIdentity(context.Background(), who))
	if !before.Allows("relay/old") || before.Allows("relay/new") || !before.AllowsAccount("relay", "first") || before.AllowsAccount("relay", "second") {
		t.Fatal("an existing request's permissions changed with its input slices")
	}
	if after.Allows("relay/old") || !after.Allows("relay/new") || after.AllowsAccount("relay", "first") || !after.AllowsAccount("relay", "second") {
		t.Fatal("a later request kept the old permissions")
	}
	before.Models = nil // magpie's own model choice still keeps the accounts
	if !before.Allows("other/model") || before.AllowsAccount("relay", "second") {
		t.Fatal("clearing models changed the account restriction")
	}
}

func TestCallerPermissionsConcurrentCopies(t *testing.T) {
	ctx := WithIdentity(context.Background(), Identity{Models: []string{"relay/*", "group/solo"}, Accounts: []string{"relay/first"}})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			who := Caller(ctx)
			for range 100 {
				if !who.Allows("relay/m") || who.Allows("other/m") || !who.AllowsAccount("relay", "first") || who.AllowsAccount("relay", "second") || !who.AllowsAccount("other", "any") {
					t.Error("a copy read different permissions")
					return
				}
			}
		})
	}
	wg.Wait()
}
