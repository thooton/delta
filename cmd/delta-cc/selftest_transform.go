package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// transformSelfTest runs transform jobs against the fake provider, with a
// stand-in for SourceForge: that a job starts a session on the first
// account in an environment that reaches only the command center and the
// package registries, made once, and installs the fonts; that its key
// reaches its document and nothing else; that a PDF submitted with its Word
// document goes on the tablet, beside the original, and both in Files; and
// that submitting or failing ends the job and its key. It needs the account
// cloudSelfTest leaves.
func transformSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token, filesDir string, rm *remarkableStore, fake *fakeCloud) error {
	fetch := transformFontSources
	defer func() { transformFontSources = fetch }()
	font := []byte("MSCF the self-test's font")
	fonts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/times32.exe":
			w.Write(font)
		case "/arial32.exe":
			w.Write([]byte("not Arial"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fonts.Close()
	transformFontSources = []string{fonts.URL + "/missing/", fonts.URL + "/"}
	pinned := transformFonts["times32.exe"]
	defer func() { transformFonts["times32.exe"] = pinned }()
	sum := sha256.Sum256(font)
	transformFonts["times32.exe"] = hex.EncodeToString(sum[:])
	stub := []byte("%PDF-1.4\n% made by the self-test\n")

	cli := 200
	if _, err := transformCLIFS.ReadFile(transformCLIPath); err != nil {
		cli = 503 // a build without it
	}
	if _, err := expect("the transform CLI needs no token", cli, "GET", "/transform/cli", "", nil); err != nil {
		return err
	}
	if data, err := expect("the fonts need no token", 200, "GET", "/transform/fonts/times32.exe", "", nil); err != nil {
		return err
	} else if !bytes.Equal(data, font) {
		return fmt.Errorf("the font: got %q", data)
	}
	if _, err := expect("only the fonts are served", 404, "GET", "/transform/fonts/other.exe", "", nil); err != nil {
		return err
	}
	if _, err := expect("a download that isn't the font is refused", 503, "GET", "/transform/fonts/arial32.exe", "", nil); err != nil {
		return err
	}
	transformFontSources = nil
	if _, err := expect("a font is kept once downloaded", 200, "GET", "/transform/fonts/times32.exe", "", nil); err != nil {
		return err
	}
	if _, err := expect("transforms require the token", 401, "GET", "/transforms", "", nil); err != nil {
		return err
	}
	folder := newRemarkableID()
	meta, err := rm.writeBlob(folder+".metadata", newMetadata("Essays", "CollectionType", ""))
	if err != nil {
		return err
	}
	if err := rm.create(folder, "", []remarkableEntry{meta}); err != nil {
		return err
	}
	doc, err := rm.add("Draft", folder, []byte("%PDF-1.4\n% the user's draft\n"))
	if err != nil {
		return err
	}
	const server = "https://cc.example/delta"
	startJob := func(desc string, formats ...string) (transformJob, string, error) {
		body, _ := json.Marshal(map[string]any{"document": doc, "server": server, "formats": formats})
		data, err := expect(desc, 201, "POST", "/transforms", token, body)
		if err != nil {
			return transformJob{}, "", err
		}
		var j transformJob
		json.Unmarshal(data, &j)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.sessions) == 0 {
			return j, "", fmt.Errorf("%s: no session started", desc)
		}
		s := fake.sessions[len(fake.sessions)-1]
		raw, _ := json.Marshal(s)
		key := regexp.MustCompile(transformKeyStart + `[0-9a-f]{64}`).FindString(string(raw))
		if j.Status != "running" || j.Session != fmt.Sprintf("session_%d", len(fake.sessions)) || j.Account != "11111111-aaaa" ||
			key == "" || j.KeyHash != "" || s["environment_id"] != "env_1" || !strings.Contains(string(raw), `Transform “Draft”`) {
			return j, "", fmt.Errorf("%s: job %+v, session %s", desc, j, raw)
		}
		return j, key, nil
	}
	first, key, err := startJob("start a transform job", "md", "DOCX", "jpg", "txt", "pdf")
	if err != nil {
		return err
	}
	if !slices.Equal(first.Formats, []string{"pdf", "docx", "md", "txt", "jpg"}) {
		return fmt.Errorf("the job's formats: got %v", first.Formats)
	}
	fake.mu.Lock()
	raw, _ := json.Marshal(fake.sessions[len(fake.sessions)-1])
	fake.mu.Unlock()
	if !strings.Contains(string(raw), "a Word document (.docx), Markdown (.md), plain text (.txt) and JPG images") {
		return fmt.Errorf("the session isn't told the formats: %s", raw)
	}
	if data, err := expect("a job's key gets its formats", 200, "GET", "/transform/job", key, nil); err != nil {
		return err
	} else if !strings.Contains(string(data), `"formats":["pdf","docx","md","txt","jpg"]`) {
		return fmt.Errorf("the job's formats: got %s", data)
	}
	fake.mu.Lock()
	envs := fake.envs
	fake.mu.Unlock()
	if len(envs) != 1 {
		return fmt.Errorf("made %d environments, want 1", len(envs))
	}
	cfg, _ := envs[0]["config"].(map[string]any)
	net, _ := cfg["network_config"].(map[string]any)
	hosts, _ := json.Marshal(net["allowed_hosts"])
	script, _ := cfg["init_script"].(string)
	vars, _ := cfg["environment"].(map[string]any)
	if string(hosts) != `["cc.example","pypi.org","files.pythonhosted.org","registry.npmjs.org"]` || net["allow_default_hosts"] != false ||
		!strings.Contains(script, server+"/transform/cli") || !strings.Contains(script, server+"/transform/fonts/") ||
		!strings.Contains(script, "times32.exe") || vars["TRANSFORM_SERVER"] != server {
		return fmt.Errorf("the environment isn't closed off: %v", envs[0])
	}

	var docx bytes.Buffer
	zw := zip.NewWriter(&docx)
	f, _ := zw.Create("word/document.xml")
	f.Write([]byte("<w:document/>"))
	zw.Close()
	jpg := []byte("\xff\xd8\xff\xe0 a page")
	steps := []struct {
		desc, method, path string
		want               int
		body               []byte
	}{
		{"a job's key reads its job", "GET", "/transform/job", 200, nil},
		{"a job's key can't list files", "GET", "/files", 403, nil},
		{"a job's key can't read the tablet", "GET", "/remarkable", 403, nil},
		{"a job's key can't read other documents", "GET", "/remarkable/" + folder, 403, nil},
		{"a job's key can't start jobs", "POST", "/transforms", 403, []byte(`{"document":"` + doc + `"}`)},
		{"a job's key isn't the delta token", "GET", "/auth", 403, nil},
		{"there is no preview", "POST", "/transform/preview", 404, stub},
		{"submit takes PDFs only", "POST", "/transform/submit", 400, []byte("hello")},
		{"a Word document comes with its PDF", "POST", "/transform/submit", 400, docx.Bytes()},
		{"a Word document alone in a form too", "POST", "/transform/submit", 400, selfTestForm("docx", docx.Bytes())},
		{"the PDF in a form is a PDF", "POST", "/transform/submit", 400, selfTestForm("pdf", []byte("hello"), "docx", docx.Bytes())},
		{"the Word document is one", "POST", "/transform/submit", 400, selfTestForm("pdf", stub, "docx", []byte("hello"))},
		{"a name the tablet can't take is refused", "POST", "/transform/submit?name=a%0Ab", 400, stub},
		{"every format asked for is submitted", "POST", "/transform/submit", 400, selfTestForm("pdf", stub, "docx", docx.Bytes())},
		{"only formats a job makes", "POST", "/transform/submit", 400, selfTestForm("pdf", stub, "gif", jpg)},
		{"an image is a JPG", "POST", "/transform/submit", 400, selfTestForm("pdf", stub, "docx", docx.Bytes(), "md", []byte("# Essay"), "txt", []byte("Essay"), "jpg", []byte("GIF89a"))},
		{"text is UTF-8", "POST", "/transform/submit", 400, selfTestForm("pdf", stub, "docx", docx.Bytes(), "md", []byte("# Essay"), "txt", []byte{0xff, 0xfe}, "jpg", jpg)},
		{"one of each but images", "POST", "/transform/submit", 400, selfTestForm("pdf", stub, "docx", docx.Bytes(), "md", []byte("# Essay"), "md", []byte("# Again"), "txt", []byte("Essay"), "jpg", jpg)},
		{"a status needs something in it", "POST", "/transform/status", 400, []byte(`{}`)},
		{"a title can't have em dashes", "POST", "/transform/status", 400, []byte(`{"title":"Essay — final"}`)},
		{"a title can't have arrows", "POST", "/transform/status", 400, []byte(`{"title":"Notes -> essay"}`)},
		{"a title is a few words", "POST", "/transform/status", 400, []byte(`{"title":"one two three four five six seven"}`)},
		{"a title can't be empty", "POST", "/transform/status", 400, []byte(`{"title":"  "}`)},
		{"a status line can't be empty", "POST", "/transform/status", 400, []byte(`{"status":""}`)},
		{"a percentage is at most 100", "POST", "/transform/status", 400, []byte(`{"percent":101}`)},
		{"a percentage is at least 0", "POST", "/transform/status", 400, []byte(`{"percent":-1}`)},
		{"set the title", "POST", "/transform/status", 200, []byte(`{"title":"MLA essay on Hamlet"}`)},
		{"set the status line and percentage", "POST", "/transform/status", 200, []byte(`{"status":"Typing up\nthe essay","percent":40}`)},
	}
	for _, s := range steps {
		if _, err := expect(s.desc, s.want, s.method, s.path, key, s.body); err != nil {
			return err
		}
	}
	if _, err := expect("the delta token isn't a job's key", 403, "GET", "/transform/job", token, nil); err != nil {
		return err
	}
	listJobs := func(desc string) ([]transformJob, []byte, error) {
		data, err := expect(desc, 200, "GET", "/transforms", token, nil)
		var jobs []transformJob
		json.Unmarshal(data, &jobs)
		return jobs, data, err
	}
	if jobs, data, err := listJobs("a running job shows its status"); err != nil {
		return err
	} else if len(jobs) != 1 || jobs[0].Title != "MLA essay on Hamlet" || jobs[0].Progress != "Typing up the essay" || jobs[0].Percent != 40 {
		return fmt.Errorf("a running job's status: got %s", data)
	}
	data, err := expect("a job's key gets its document", 200, "GET", "/transform/document", key, nil)
	if err != nil {
		return err
	}
	if zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err != nil || !slices.ContainsFunc(zr.File, func(f *zip.File) bool { return f.Name == doc+".pdf" }) {
		return fmt.Errorf("the job's document: %v", err)
	}
	data, err = expect("submit every format", 200, "POST", "/transform/submit?name=Essay.docx", key,
		selfTestForm("pdf", stub, "docx", docx.Bytes(), "md", []byte("# Essay"), "txt", []byte("Essay"), "jpg", jpg, "jpg", jpg))
	if err != nil {
		return err
	}
	var result struct {
		Name, Tablet string
		Files        []string
	}
	json.Unmarshal(data, &result)
	if result.Name != "Essay" || !slices.Equal(result.Files, []string{"Essay.pdf", "Essay.docx", "Essay.md", "Essay.txt", "Essay 1.jpg", "Essay 2.jpg"}) {
		return fmt.Errorf("submit: got %s", data)
	}
	got, err := rm.find(result.Tablet)
	if err != nil || got.Name != "Essay" || got.Parent != folder || got.Type != "pdf" {
		return fmt.Errorf("the result on the tablet: %+v (%v)", got, err)
	}
	for name, want := range map[string][]byte{"Essay.pdf": stub, "Essay.docx": docx.Bytes(), "Essay.md": []byte("# Essay"), "Essay 2.jpg": jpg} {
		if saved, err := os.ReadFile(filepath.Join(filesDir, name)); err != nil || !bytes.Equal(saved, want) {
			return fmt.Errorf("%s in Files: %v", name, err)
		}
	}
	if _, err := expect("a finished job's key is dead", 401, "GET", "/transform/job", key, nil); err != nil {
		return err
	}

	second, key, err := startJob("start another")
	if err != nil {
		return err
	}
	if fake.mu.Lock(); len(fake.envs) != 1 {
		fake.mu.Unlock()
		return fmt.Errorf("the environment was made again")
	}
	fake.mu.Unlock()
	if _, err := expect("title the second job", 200, "POST", "/transform/status", key, []byte(`{"title":"Study guide"}`)); err != nil {
		return err
	}
	if _, err := expect("a format not asked for is refused", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "docx", docx.Bytes())); err != nil {
		return err
	}
	data, err = expect("submit a PDF without a name", 200, "POST", "/transform/submit", key, stub)
	if err != nil {
		return err
	}
	var titled struct{ Files []string }
	json.Unmarshal(data, &titled)
	if !slices.Equal(titled.Files, []string{"Study guide.pdf"}) {
		return fmt.Errorf("a result without a name isn't named after the title: %s", data)
	}

	third, key, err := startJob("start a third", "eml")
	if err != nil {
		return err
	}
	for _, s := range []struct {
		desc string
		body []byte
	}{
		{"an email's headers parse", selfTestForm("eml", []byte("not an email"))},
		{"an email has a subject", selfTestForm("eml", []byte("To: a@b.org\n\nHi"))},
		{"an email has no From", selfTestForm("eml", []byte("From: a@b.org\nSubject: Hi\n\nHi"))},
		{"an email's addresses are addresses", selfTestForm("eml", []byte("To: Ms. Rivera\nSubject: Hi\n\nHi"))},
		{"a PDF not asked for is refused", selfTestForm("pdf", stub, "eml", []byte("Subject: Hi\n\nHi"))},
		{"an email is plain text", selfTestForm("eml", []byte("Subject: Hi\nContent-Type: text/html\n\n<p>Hi"))},
	} {
		if _, err := expect(s.desc, 400, "POST", "/transform/submit", key, s.body); err != nil {
			return err
		}
	}
	if _, err := expect("fail without a reason", 400, "POST", "/transform/fail", key, []byte(`{}`)); err != nil {
		return err
	}
	if _, err := expect("fail", 200, "POST", "/transform/fail", key, []byte(`{"reason":"There are no instructions on it."}`)); err != nil {
		return err
	}
	if _, err := expect("a failed job's key is dead", 401, "POST", "/transform/submit", key, stub); err != nil {
		return err
	}
	jobs, data, err := listJobs("list transform jobs")
	if err != nil {
		return err
	}
	if len(jobs) != 3 || jobs[0].ID != third.ID || jobs[0].Status != "failed" || jobs[0].Error != "There are no instructions on it." ||
		jobs[1].ID != second.ID || jobs[1].Result != "Study guide" ||
		jobs[2].ID != first.ID || jobs[2].Status != "done" || jobs[2].Result != "Essay" || jobs[2].Percent != 100 || jobs[2].Progress != "" ||
		strings.Contains(string(data), "key_hash") {
		return fmt.Errorf("list transform jobs: got %s", data)
	}
	for _, s := range []struct {
		desc string
		want int
		body string
	}{
		{"a folder can't be transformed", 400, `{"document":"` + folder + `","server":"` + server + `"}`},
		{"a missing document can't be transformed", 404, `{"document":"nope","server":"` + server + `"}`},
		{"only formats a job makes", 400, `{"document":"` + doc + `","server":"` + server + `","formats":["gif"]}`},
		{"the server must be https", 400, `{"document":"` + doc + `","server":"http://cc.example"}`},
	} {
		if _, err := expect(s.desc, s.want, "POST", "/transforms", token, []byte(s.body)); err != nil {
			return err
		}
	}
	for _, j := range jobs {
		if _, err := expect("forget a job", 200, "DELETE", "/transforms/"+j.ID, token, nil); err != nil {
			return err
		}
	}
	for _, name := range append(result.Files, titled.Files...) {
		if _, err := expect("delete a result", 200, "DELETE", "/files/"+name, token, nil); err != nil {
			return err
		}
	}
	return nil
}

const selfTestBoundary = "delta-self-test"

// selfTestForm is a multipart/form-data body of name, value pairs, which
// the self-test's requests send as one.
func selfTestForm(fields ...any) []byte {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	w.SetBoundary(selfTestBoundary)
	for i := 0; i+1 < len(fields); i += 2 {
		part, _ := w.CreateFormFile(fields[i].(string), fields[i].(string))
		part.Write(fields[i+1].([]byte))
	}
	w.Close()
	return b.Bytes()
}
