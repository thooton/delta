package main

import (
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
		{"    Error: No Soulseek file results found.", "    Error: No file results found.", true},
		{"[002] SongJob: succeeded: A - B: noctower\\..\\Daft Punk\\Discovery\\03 - B.mp3", "[002] SongJob: succeeded: A - B: 03 - B.mp3", true},
		{"[003] AlbumJob: downloading: A - X: bob\\Music\\A\\X", "[003] AlbumJob: downloading: A - X: X", true},
		{"  Reason:    30000ms without peer transfer activity: bob\\x.mp3", "  Reason: the download failed", true},
		{"[warn] Too many uploads queued by user bob", "", false},
		{"[debug] anything", "", false},
		{"Logged in as secretuser", "Logged in as ", true},
		{"[001] ExtractJob: Input: slsk://bob/Music/a b/", "[001] ExtractJob: Input: a b", true},
		{"[001] ExtractJob: Input: slsk://bob/Music/a/b.mp3", "[001] ExtractJob: Input: b.mp3", true},
	}
	for _, c := range cases {
		got, keep := musicLogLine(c.in, "secretuser")
		if got != c.want || keep != c.keep {
			t.Errorf("musicLogLine(%q) = %q, %v; want %q, %v", c.in, got, keep, c.want, c.keep)
		}
	}
}

func TestMusicArgs(t *testing.T) {
	for _, args := range [][]string{
		{"search", "x"}, {"get", "x"}, {"config"},
		{"song"}, {"album", " "},
		{"song", "--format", "flac", "A - B"}, {"album", "A - B", "-n", "3"},
	} {
		if err := runMusic(args); err == nil {
			t.Errorf("runMusic(%q) accepted", args)
		}
	}
}
