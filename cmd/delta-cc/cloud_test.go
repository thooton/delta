package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// A new session goes to the first account with room that has cloud credits
// left, or else to the first account with room.
func TestCloudPick(t *testing.T) {
	usage := map[string]string{
		"full":    `{"five_hour":{"utilization":0},"seven_day":{"utilization":100},"` + cloudCreditsKey + `":{"remaining_dollars":50}}`,
		"spent":   `{"five_hour":{"utilization":40},"seven_day":{"utilization":6},"` + cloudCreditsKey + `":{"remaining_dollars":0}}`,
		"none":    `{"five_hour":{"utilization":10},"seven_day":{"utilization":10},"` + cloudCreditsKey + `":null}`,
		"credits": `{"five_hour":{"utilization":20},"seven_day":{"utilization":50},"` + cloudCreditsKey + `":{"remaining_dollars":157.5}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(usage[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]))
	}))
	defer srv.Close()
	s := newCloudAccountStore(filepath.Join(t.TempDir(), cloudAccountsFile))
	s.apiURL = srv.URL
	later := time.Now().Add(time.Hour).UnixMilli()
	for _, c := range []struct {
		accounts []string
		want     string
	}{
		{[]string{"full", "spent", "none", "credits"}, "credits"},
		{[]string{"full", "spent", "none"}, "spent"},
		{[]string{"full"}, ""},
	} {
		var accounts []cloudAccount
		for _, id := range c.accounts {
			accounts = append(accounts, cloudAccount{ID: id, AccessToken: id, RefreshToken: "rt", ExpiresAt: later})
		}
		if err := s.save(accounts); err != nil {
			t.Fatal(err)
		}
		got, err := s.pick(context.Background())
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("%v: got %q, %v; want %q", c.accounts, got, err, c.want)
		}
	}
}
