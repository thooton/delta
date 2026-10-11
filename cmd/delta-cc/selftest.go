package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
)

// selfTest runs the real handler against a temporary data directory and
// checks the basics. An update only installs a binary whose selfTest passes,
// so keep it covering anything that must never break.
func selfTest() error {
	dir, err := os.MkdirTemp("", "delta-cc-test-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	filesDir := filepath.Join(dir, "files")
	if err := os.Mkdir(filesDir, 0o700); err != nil {
		return err
	}

	const token = "delta_selftest"
	hash := sha256.Sum256([]byte(token))
	u := &updater{restart: make(chan struct{}, 1)}
	ts := httptest.NewServer(newHandler(hash[:], filesDir, 1<<20, u, nil))
	defer ts.Close()

	call := func(method, path, tok string, body []byte) (int, []byte, error) {
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return resp.StatusCode, data, err
	}
	expect := func(desc string, want int, method, path, tok string, body []byte) ([]byte, error) {
		got, data, err := call(method, path, tok, body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", desc, err)
		}
		if got != want {
			return nil, fmt.Errorf("%s: got HTTP %d, want %d (%s)", desc, got, want, strings.TrimSpace(string(data)))
		}
		return data, nil
	}

	content := []byte("hello from the delta-cc self-test\n")
	steps := []struct {
		desc         string
		want         int
		method, path string
		tok          string
		body         []byte
	}{
		{"no token is rejected", 401, "GET", "/files", "", nil},
		{"wrong token is rejected", 401, "GET", "/files", "nope", nil},
		{"auth check", 200, "GET", "/auth", token, nil},
		{"version", 200, "GET", "/version", token, nil},
		{"upload new file", 201, "PUT", "/files/test.apk", token, content},
		{"replace file", 200, "PUT", "/files/test.apk", token, content},
		{"oversized upload is rejected", 413, "PUT", "/files/big.bin", token, make([]byte, 1<<20+1)},
		{"hidden name is rejected", 400, "PUT", "/files/.hidden", token, content},
		{"path traversal is rejected", 400, "PUT", "/files/..%2Fescape", token, content},
		{"update without a URL is rejected", 400, "POST", "/update", token, []byte("{}")},
		{"missing file download", 404, "GET", "/files/missing", token, nil},
		{"canvas relay requires the token", 401, "GET", "/canvas/api/v1/users/self", "", nil},
		{"unconfigured canvas relay says so", 503, "GET", "/canvas/api/v1/users/self", token, nil},
	}
	for _, s := range steps {
		if _, err := expect(s.desc, s.want, s.method, s.path, s.tok, s.body); err != nil {
			return err
		}
	}

	data, err := expect("download", 200, "GET", "/files/test.apk", token, nil)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, content) {
		return fmt.Errorf("download: content does not match upload")
	}

	data, err = expect("list", 200, "GET", "/files", token, nil)
	if err != nil {
		return err
	}
	var files []fileInfo
	if err := json.Unmarshal(data, &files); err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if len(files) != 1 || files[0].Name != "test.apk" || files[0].Size != int64(len(content)) {
		return fmt.Errorf("list: unexpected result %s", data)
	}

	if _, err := expect("delete", 200, "DELETE", "/files/test.apk", token, nil); err != nil {
		return err
	}
	if _, err := expect("delete again", 404, "DELETE", "/files/test.apk", token, nil); err != nil {
		return err
	}
	if entries, _ := os.ReadDir(filesDir); len(entries) != 0 {
		return fmt.Errorf("files directory not empty after delete (leftover temp files?)")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		return fmt.Errorf("path traversal wrote outside the files directory")
	}
	return canvasSelfTest(dir)
}
