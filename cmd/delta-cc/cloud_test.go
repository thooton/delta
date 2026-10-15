package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// Many callers turning down the same token at once renew the login once, and
// all of them end up with the renewed token.
func TestCloudRenewsOnce(t *testing.T) {
	fake := newFakeCloud()
	defer fake.Close()
	s := newCloudAccountStore(filepath.Join(t.TempDir(), cloudAccountsFile))
	s.tokenURL, s.apiURL = fake.URL+"/v1/oauth/token", fake.URL
	a := cloudAccount{RefreshToken: "rt-1"}
	if err := s.identify(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	if err := s.save([]cloudAccount{a}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := s.fresh(context.Background(), a.ID, "at-2")
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = b.AccessToken
		}()
	}
	wg.Wait()
	for _, g := range got {
		if g != "at-3" {
			t.Fatalf("got %q, want at-3", got)
		}
	}
	if fake.renewals != 2 {
		t.Fatalf("%d renewals, want 2 (one to add, one after the refusal)", fake.renewals)
	}
}

// A login the provider has dropped is reported as rejected, not as a fault.
func TestCloudRejected(t *testing.T) {
	fake := newFakeCloud()
	defer fake.Close()
	s := newCloudAccountStore(filepath.Join(t.TempDir(), cloudAccountsFile))
	s.tokenURL, s.apiURL = fake.URL+"/v1/oauth/token", fake.URL
	if err := s.save([]cloudAccount{{ID: "x", RefreshToken: "rt-0"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.fresh(context.Background(), "x", ""); err != errCloudRejected {
		t.Fatalf("got %v, want errCloudRejected", err)
	}
	if _, err := s.fresh(context.Background(), "y", ""); err != errCloudUnknown {
		t.Fatalf("got %v, want errCloudUnknown", err)
	}
}
