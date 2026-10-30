package main

import (
	"archive/zip"
	"bytes"
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
func transformSelfTest(expect expectFunc, token, filesDir string, rm *remarkableStore, fake *fakeCloud) error {
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
	transformFonts["times32.exe"] = keyHash(string(font))
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
	if err := runSteps(expect,
		step{"only the fonts are served", 404, "GET", "/transform/fonts/other.exe", "", nil},
		step{"a download that isn't the font is refused", 503, "GET", "/transform/fonts/arial32.exe", "", nil},
	); err != nil {
		return err
	}
	transformFontSources = nil
	if err := runSteps(expect,
		step{"a font is kept once downloaded", 200, "GET", "/transform/fonts/times32.exe", "", nil},
		step{"transforms require the token", 401, "GET", "/transforms", "", nil},
	); err != nil {
		return err
	}
	folder := randomUUID()
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
		var j transformJob
		if _, err := expectJSON(expect, &j, desc, 201, "POST", "/transforms", token, jsonBody(map[string]any{"document": doc, "server": server, "formats": formats})); err != nil {
			return transformJob{}, "", err
		}
		_, sessions := fake.made()
		if len(sessions) == 0 {
			return j, "", fmt.Errorf("%s: no session started", desc)
		}
		s := sessions[len(sessions)-1]
		raw, _ := json.Marshal(s)
		key := regexp.MustCompile(transformKeyStart + `[0-9a-f]{64}`).FindString(string(raw))
		if j.Status != "running" || j.Session != fmt.Sprintf("session_%d", len(sessions)) || j.Account != "11111111-aaaa" ||
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
	_, sessions := fake.made()
	raw, _ := json.Marshal(sessions[len(sessions)-1])
	if !strings.Contains(string(raw), "a Word document (.docx), Markdown (.md), plain text (.txt) and JPG images") {
		return fmt.Errorf("the session isn't told the formats: %s", raw)
	}
	if data, err := expect("a job's key gets its formats", 200, "GET", "/transform/job", key, nil); err != nil {
		return err
	} else if !strings.Contains(string(data), `"formats":["pdf","docx","md","txt","jpg"]`) {
		return fmt.Errorf("the job's formats: got %s", data)
	}
	envs, _ := fake.made()
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
	if err := runSteps(expect,
		step{"a job's key reads its job", 200, "GET", "/transform/job", key, nil},
		step{"a job's key can't list files", 403, "GET", "/files", key, nil},
		step{"a job's key can't read the tablet", 403, "GET", "/remarkable", key, nil},
		step{"a job's key can't read other documents", 403, "GET", "/remarkable/" + folder, key, nil},
		step{"a job's key can't start jobs", 403, "POST", "/transforms", key, []byte(`{"document":"` + doc + `"}`)},
		step{"a job's key isn't the delta token", 403, "GET", "/auth", key, nil},
		step{"there is no preview", 404, "POST", "/transform/preview", key, stub},
		step{"a job's key reaches the shorthand reader (offline here)", 503, "POST", "/transform/shorthand", key, []byte("\x89PNG\r\n\x1a\n a page")},
		step{"a job's key fetches only readings it submitted", 404, "GET", "/transform/shorthand/0123", key, nil},
		step{"submit takes PDFs only", 400, "POST", "/transform/submit", key, []byte("hello")},
		step{"a Word document comes with its PDF", 400, "POST", "/transform/submit", key, docx.Bytes()},
		step{"a Word document alone in a form too", 400, "POST", "/transform/submit", key, selfTestForm("docx", docx.Bytes())},
		step{"the PDF in a form is a PDF", 400, "POST", "/transform/submit", key, selfTestForm("pdf", []byte("hello"), "docx", docx.Bytes())},
		step{"the Word document is one", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "docx", []byte("hello"))},
		step{"a name the tablet can't take is refused", 400, "POST", "/transform/submit?name=a%0Ab", key, stub},
		step{"every format asked for is submitted", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "docx", docx.Bytes())},
		step{"only formats a job makes", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "gif", jpg)},
		step{"an image is a JPG", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "docx", docx.Bytes(), "md", []byte("# Essay"), "txt", []byte("Essay"), "jpg", []byte("GIF89a"))},
		step{"text is UTF-8", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "docx", docx.Bytes(), "md", []byte("# Essay"), "txt", []byte{0xff, 0xfe}, "jpg", jpg)},
		step{"one of each but images", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "docx", docx.Bytes(), "md", []byte("# Essay"), "md", []byte("# Again"), "txt", []byte("Essay"), "jpg", jpg)},
		step{"a status needs something in it", 400, "POST", "/transform/status", key, []byte(`{}`)},
		step{"a title can't have em dashes", 400, "POST", "/transform/status", key, []byte(`{"title":"Essay — final"}`)},
		step{"a title can't have arrows", 400, "POST", "/transform/status", key, []byte(`{"title":"Notes -> essay"}`)},
		step{"a title is a few words", 400, "POST", "/transform/status", key, []byte(`{"title":"one two three four five six seven"}`)},
		step{"a title can't be empty", 400, "POST", "/transform/status", key, []byte(`{"title":"  "}`)},
		step{"a status line can't be empty", 400, "POST", "/transform/status", key, []byte(`{"status":""}`)},
		step{"a percentage is at most 100", 400, "POST", "/transform/status", key, []byte(`{"percent":101}`)},
		step{"a percentage is at least 0", 400, "POST", "/transform/status", key, []byte(`{"percent":-1}`)},
		step{"set the title", 200, "POST", "/transform/status", key, []byte(`{"title":"MLA essay on Hamlet"}`)},
		step{"set the status line and percentage", 200, "POST", "/transform/status", key, []byte(`{"status":"Typing up\nthe essay","percent":40}`)},
		step{"the delta token isn't a job's key", 403, "GET", "/transform/job", token, nil},
	); err != nil {
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
	var result struct {
		Name, Tablet string
		Files        []string
	}
	if data, err = expectJSON(expect, &result, "submit every format", 200, "POST", "/transform/submit?name=Essay.docx", key,
		selfTestForm("pdf", stub, "docx", docx.Bytes(), "md", []byte("# Essay"), "txt", []byte("Essay"), "jpg", jpg, "jpg", jpg)); err != nil {
		return err
	}
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
	if envs, _ := fake.made(); len(envs) != 1 {
		return fmt.Errorf("the environment was made again")
	}
	if err := runSteps(expect,
		step{"title the second job", 200, "POST", "/transform/status", key, []byte(`{"title":"Study guide"}`)},
		step{"a format not asked for is refused", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "docx", docx.Bytes())},
	); err != nil {
		return err
	}
	var titled struct{ Files []string }
	if data, err = expectJSON(expect, &titled, "submit a PDF without a name", 200, "POST", "/transform/submit", key, stub); err != nil {
		return err
	}
	if !slices.Equal(titled.Files, []string{"Study guide.pdf"}) {
		return fmt.Errorf("a result without a name isn't named after the title: %s", data)
	}

	third, key, err := startJob("start a third", "eml")
	if err != nil {
		return err
	}
	if err := runSteps(expect,
		step{"an email's headers parse", 400, "POST", "/transform/submit", key, selfTestForm("eml", []byte("not an email"))},
		step{"an email has a subject", 400, "POST", "/transform/submit", key, selfTestForm("eml", []byte("To: a@b.org\n\nHi"))},
		step{"an email has no From", 400, "POST", "/transform/submit", key, selfTestForm("eml", []byte("From: a@b.org\nSubject: Hi\n\nHi"))},
		step{"an email's addresses are addresses", 400, "POST", "/transform/submit", key, selfTestForm("eml", []byte("To: Ms. Rivera\nSubject: Hi\n\nHi"))},
		step{"a PDF not asked for is refused", 400, "POST", "/transform/submit", key, selfTestForm("pdf", stub, "eml", []byte("Subject: Hi\n\nHi"))},
		step{"an email is plain text", 400, "POST", "/transform/submit", key, selfTestForm("eml", []byte("Subject: Hi\nContent-Type: text/html\n\n<p>Hi"))},
		step{"flashcards not asked for are refused", 400, "POST", "/transform/submit", key, selfTestForm("eml", []byte("Subject: Hi\n\nHi"), "fkv", []byte("hola||hello\n"))},
		step{"fail without a reason", 400, "POST", "/transform/fail", key, []byte(`{}`)},
		step{"fail", 200, "POST", "/transform/fail", key, []byte(`{"reason":"There are no instructions on it."}`)},
		step{"a failed job's key is dead", 401, "POST", "/transform/submit", key, stub},
	); err != nil {
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
	steps := []step{
		{"a folder can't be transformed", 400, "POST", "/transforms", token, []byte(`{"document":"` + folder + `","server":"` + server + `"}`)},
		{"a missing document can't be transformed", 404, "POST", "/transforms", token, []byte(`{"document":"nope","server":"` + server + `"}`)},
		{"only formats a job makes", 400, "POST", "/transforms", token, []byte(`{"document":"` + doc + `","server":"` + server + `","formats":["gif"]}`)},
		{"the server must be https", 400, "POST", "/transforms", token, []byte(`{"document":"` + doc + `","server":"http://cc.example"}`)},
	}
	for _, j := range jobs {
		steps = append(steps, step{"forget a job", 200, "DELETE", "/transforms/" + j.ID, token, nil})
	}
	for _, name := range append(result.Files, titled.Files...) {
		steps = append(steps, step{"delete a result", 200, "DELETE", "/files/" + name, token, nil})
	}
	return runSteps(expect, steps...)
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
