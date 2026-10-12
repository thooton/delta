package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestMusicConfigWithLogin(t *testing.T) {
	in := "output-dir = ~/Music\nUsername = theirs\n[albums]\npassword=secret\nprofile-cond = album\n"
	got := musicConfigWithLogin(in, "u1", "p1")
	if !strings.HasPrefix(got, "username = u1\npassword = p1\n") {
		t.Fatalf("login not first:\n%s", got)
	}
	if strings.Contains(got, "theirs") || strings.Contains(got, "secret") {
		t.Errorf("config's own login kept:\n%s", got)
	}
	if !strings.Contains(got, "[albums]\nprofile-cond = album\n") {
		t.Errorf("profile lost:\n%s", got)
	}
}

func TestMusicConfigOutputDir(t *testing.T) {
	if got := musicConfigOutputDir("format = mp3\noutput-dir = \"~/a b\"\n[x]\noutput-dir = /no\n"); got != "~/a b" {
		t.Errorf("got %q", got)
	}
	if got := musicConfigOutputDir("[x]\noutput-dir = /no\n"); got != "." {
		t.Errorf("profile output-dir used: %q", got)
	}
}

func TestMusicLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a1, release, err := musicLogin()
	if err != nil {
		t.Fatal(err)
	}
	release()
	a2, release, err := musicLogin()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if a1 != a2 {
		t.Errorf("login not kept: %+v then %+v", a1, a2)
	}
	n := newMusicAccount()
	if len(a1.Username) != 16 || len(a1.Password) != 24 || n == a1 || a1.Username[0] < 'a' || a1.Username[0] > 'z' {
		t.Errorf("bad logins %+v %+v", a1, n)
	}
}

func TestMusicLogLine(t *testing.T) {
	cases := []struct {
		in, want string
		keep     bool
	}{
		{"[cli] Starting CLI session in local mode", "", false},
		{"[soulseek] Logging in as abc..", "", false},
		{"[warn] [soulseek] Connection lost. Retrying in 1s...", "", false},
		{"   at Soulseek.SoulseekClient.ConnectInternalAsync()", "", false},
		{"[cli] Completed: 1 succeeded, 0 failed.", "Completed: 1 succeeded, 0 failed.", true},
		{"[error] [cli] Input error: Unknown argument: --x", "error: Unknown argument: --x", true},
		{"\x1b[32m[002] SongJob: succeeded: A - B\x1b[0m", "[002] SongJob: succeeded: A - B", true},
		{"Wrote the Sockseek index for secretuser", "Wrote the delta music index for (delta)", true},
	}
	for _, c := range cases {
		got, keep := musicLogLine(c.in, "secretuser")
		if got != c.want || keep != c.keep {
			t.Errorf("musicLogLine(%q) = %q, %v; want %q, %v", c.in, got, keep, c.want, c.keep)
		}
	}
}

func TestMusicEngineArgs(t *testing.T) {
	o, err := parseOpts([]string{"x", "-o", "/music", "--format=flac", "-d", "--strict-title", "-n", "3", "--dry-run"},
		musicValueFlags+" "+musicOwnValueFlags, musicBoolFlags+" "+musicOwnBoolFlags+" album")
	if err != nil {
		t.Fatal(err)
	}
	got, err := musicEngineArgs(o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--format", "flac", "--number", "3", "--output-dir", "/music", "--desperate", "--strict-title"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if _, err := parseOpts([]string{"--config", "x"}, musicValueFlags, musicBoolFlags); err == nil {
		t.Error("--config accepted")
	}
}

func TestMusicHits(t *testing.T) {
	results := []musicResult{
		{User: musicUser{Username: "u", HasFreeUploadSlot: true},
			File: &musicFile{Filename: `m\A\B\01 X.MP3`, Size: 100, Bitrate: 320, Length: 61}},
		{User: musicUser{Username: "v"}, Files: []musicFile{
			{Filename: `m\A\Album\01.mp3`, Size: 10, Bitrate: 320, Length: 100},
			{Filename: `m\A\Album\CD2\02.flac`, Size: 20, Bitrate: 0, Length: 50},
		}},
	}
	hits := musicHits(results)
	if len(hits) != 2 {
		t.Fatalf("got %d hits", len(hits))
	}
	if h := hits[0]; h.Link != "slsk://u/m/A/B/01 X.MP3" || h.Format != "mp3" || !h.FreeSlot {
		t.Errorf("song hit %+v", h)
	}
	if h := hits[1]; h.Link != "slsk://v/m/A/Album/" || h.Tracks != 2 || h.Bytes != 30 || h.Seconds != 150 || h.Bitrate != 320 || h.Format != "flac,mp3" {
		t.Errorf("album hit %+v", h)
	}
}
