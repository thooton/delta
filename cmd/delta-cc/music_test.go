package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestMusicRelativeDataDir checks that a relative -data (the default) still
// gives the engine absolute paths, since it runs in another directory.
func TestMusicRelativeDataDir(t *testing.T) {
	t.Chdir(t.TempDir())
	m := newMusicService("data")
	if !filepath.IsAbs(m.dataDir) || !filepath.IsAbs(m.config.path) {
		t.Errorf("relative paths kept: %q, %q", m.dataDir, m.config.path)
	}
}

func TestMusicAccount(t *testing.T) {
	m := newMusicService(t.TempDir())
	a1, err := m.account(false)
	if err != nil {
		t.Fatal(err)
	}
	if a2, err := m.account(false); err != nil || a1 != a2 {
		t.Errorf("login not kept: %+v then %+v (%v)", a1, a2, err)
	}
	a3, err := m.account(true)
	if err != nil {
		t.Fatal(err)
	}
	if a4, _ := m.account(false); a4 != a3 || a3 == a1 {
		t.Errorf("renewed login not kept: %+v, %+v, %+v", a1, a3, a4)
	}
	if len(a1.Username) != 16 || len(a1.Password) != 24 || a1.Username[0] < 'a' || a1.Username[0] > 'z' {
		t.Errorf("bad login %+v", a1)
	}
}

// TestMusicJob checks that a job's files, and only those, come back as a
// tar, and that deleting the job removes them.
func TestMusicJob(t *testing.T) {
	dir := t.TempDir()
	m := newMusicService(dir)
	mux := http.NewServeMux()
	m.register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	const id = "00112233445566778899aabbccddeeff"
	job := filepath.Join(dir, musicJobsDir, id)
	files := map[string]string{
		"A - X/01 - One.mp3": "one",
		"A - X/cover.jpg":    "jpg",
		"B - Ünïcode.mp3":    "two",
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(job, name)), 0o700)
		os.WriteFile(filepath.Join(job, name), []byte(body), 0o600)
	}
	for _, junk := range []string{"_index.csv", "A - X/02 - Two.mp3.incomplete", ".cache/x"} {
		os.MkdirAll(filepath.Dir(filepath.Join(job, junk)), 0o700)
		os.WriteFile(filepath.Join(job, junk), []byte("junk"), 0o600)
	}
	if got := musicJobFiles(job); len(got) != len(files) {
		t.Errorf("musicJobFiles = %+v", got)
	}

	resp, err := http.Get(ts.URL + "/music-jobs/" + id)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	tr := tar.NewReader(bytes.NewReader(data))
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		got[h.Name] = string(body)
	}
	if len(got) != len(files) {
		t.Errorf("tar has %v, want %v", got, files)
	}
	for name, body := range files {
		if got[name] != body {
			t.Errorf("tar %q = %q, want %q", name, got[name], body)
		}
	}

	req, _ := http.NewRequest("DELETE", ts.URL+"/music-jobs/"+id, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("delete: %v %v", resp.Status, err)
	}
	if _, err := os.Stat(job); err == nil {
		t.Error("job still there after delete")
	}
}

// TestMusicRunRejects checks that bad requests are refused before anything
// is streamed.
func TestMusicRunRejects(t *testing.T) {
	m := newMusicService(t.TempDir())
	for _, body := range []string{`{"mode":"x","query":"a"}`, `{"mode":"song","query":"-a"}`, `{"mode":"song","query":"a\nb"}`, `[]`} {
		w := httptest.NewRecorder()
		m.run(w, httptest.NewRequest("POST", "/music", strings.NewReader(body)))
		var e struct{ Error string }
		if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Error == "" {
			t.Errorf("%s: got %d %s", body, w.Code, w.Body)
		}
	}
}

func TestMusicLogLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		keep bool
	}{
		{"[cli] Starting CLI session in local mode", nil, false},
		{"[soulseek] Logging in as abc..", nil, false},
		{"[warn] [soulseek] Connection lost. Retrying in 1s...", nil, false},
		{"   at Soulseek.SoulseekClient.ConnectInternalAsync()", nil, false},
		{"[cli] Completed: 1 succeeded, 0 failed.", []string{"Done: 1 downloaded, 0 failed."}, true},
		{"[error] [cli] Input error: Unknown argument: --x", []string{"error: Unknown argument: --x"}, true},
		{"\x1b[32m[002] SongJob: succeeded: A - B\x1b[0m", []string{`Downloaded "A - B".`}, true},
		{"    Error: No Soulseek file results found.", []string{"    Error: No match found on Amazon Music."}, true},
		{"[002] SongJob: succeeded: A - B: noctower\\..\\Daft Punk\\Discovery\\03 - B.mp3", []string{`Downloaded "03 - B.mp3".`}, true},
		{"[003] AlbumJob: downloading: A - X: bob\\Music\\A\\X", []string{`Purchasing album "X"...`, `Downloading "X"...`}, true},
		{"[002] SongJob: downloading: A - B: bob\\Music\\03 - B.mp3", []string{`Purchasing "03 - B"...`, `Downloading "03 - B.mp3"...`}, true},
		{"[002] SongJob: searching: A - B", []string{`Searching Amazon Music for "A - B"...`}, true},
		{"[003] AlbumJob: searching: A - X", []string{`Searching Amazon Music for album "A - X"...`}, true},
		{"[002] SongJob: downloading: A - B (3 peers)", []string{`Purchasing "A - B (3 results)"...`, `Downloading "A - B (3 results)"...`}, true},
		{"[003] AlbumJob: queued (r): A - X", []string{`Waiting for "A - X"...`}, true},
		{"[002] SongJob: failed [No search results]: A - B", []string{`No match found for "A - B" on Amazon Music.`}, true},
		{"[003] AlbumJob: failed: A - X", []string{`No match found for album "A - X" on Amazon Music.`}, true},
		{"[002] SongJob: browsing: A - B", []string{`Working on "A - B"...`}, true},
		{"[005] SearchJob: searching: A - B", nil, false},
		{"[001] ExtractJob: String: Input: Artist - Title", nil, false},
		{"  Reason:    30000ms without peer transfer activity: bob\\x.mp3", []string{"  Reason: the download failed"}, true},
		{"[warn] Too many uploads queued by user bob", nil, false},
		{"[debug] anything", nil, false},
		{"Logged in as secretuser", []string{"Logged in as "}, true},
		{"[001] ExtractJob: Input: slsk://bob/Music/a b/", nil, false},
		{"[001] ExtractJob: Input: slsk://bob/Music/a/b.mp3", nil, false},
	}
	for _, c := range cases {
		got, keep := musicLogLine(c.in, "secretuser")
		if keep != c.keep || len(got) != len(c.want) {
			t.Errorf("musicLogLine(%q) = %q, %v; want %q, %v", c.in, got, keep, c.want, c.keep)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("musicLogLine(%q)[%d] = %q; want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
