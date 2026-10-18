package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// transformSelfTest runs transform jobs against the fake provider, with a
// stand-in for LibreOffice: that a job starts a session on the first
// account in an environment that reaches only the command center and the
// package registries, made once; that its key reaches its document and
// nothing else; that a Word document comes back as a PDF on the tablet,
// beside the original, and both in Files; and that submitting or failing
// ends the job and its key. It needs the account cloudSelfTest leaves.
func transformSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token, filesDir string, rm *remarkableStore, fake *fakeCloud) error {
	convert := transformConvert
	defer func() { transformConvert = convert }()
	stub := []byte("%PDF-1.4\n% converted by the self-test\n")
	transformConvert = func(ctx context.Context, docx []byte) ([]byte, error) { return stub, nil }

	cli := 200
	if _, err := transformCLIFS.ReadFile(transformCLIPath); err != nil {
		cli = 503 // a build without it
	}
	if _, err := expect("the transform CLI needs no token", cli, "GET", "/transform/cli", "", nil); err != nil {
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
	startJob := func(desc string) (transformJob, string, error) {
		body, _ := json.Marshal(map[string]string{"document": doc, "server": server})
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
	first, key, err := startJob("start a transform job")
	if err != nil {
		return err
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
		!strings.Contains(script, server+"/transform/cli") || vars["TRANSFORM_SERVER"] != server {
		return fmt.Errorf("the environment isn't closed off: %v", envs[0])
	}

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
		{"preview takes Word documents only", "POST", "/transform/preview", 400, stub},
		{"submit takes PDFs and Word documents only", "POST", "/transform/submit", 400, []byte("hello")},
		{"a name the tablet can't take is refused", "POST", "/transform/submit?name=a%0Ab", 400, stub},
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
	var docx bytes.Buffer
	zw := zip.NewWriter(&docx)
	f, _ := zw.Create("word/document.xml")
	f.Write([]byte("<w:document/>"))
	zw.Close()
	if data, err := expect("preview a Word document", 200, "POST", "/transform/preview", key, docx.Bytes()); err != nil {
		return err
	} else if !bytes.Equal(data, stub) {
		return fmt.Errorf("preview: got %q", data)
	}
	data, err = expect("submit a Word document", 200, "POST", "/transform/submit?name=Essay.docx", key, docx.Bytes())
	if err != nil {
		return err
	}
	var result struct {
		Name, Tablet string
		Files        []string
	}
	json.Unmarshal(data, &result)
	if result.Name != "Essay" || !slices.Equal(result.Files, []string{"Essay.pdf", "Essay.docx"}) {
		return fmt.Errorf("submit: got %s", data)
	}
	got, err := rm.find(result.Tablet)
	if err != nil || got.Name != "Essay" || got.Parent != folder || got.Type != "pdf" {
		return fmt.Errorf("the result on the tablet: %+v (%v)", got, err)
	}
	for name, want := range map[string][]byte{"Essay.pdf": stub, "Essay.docx": docx.Bytes()} {
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
	data, err = expect("submit a PDF without a name", 200, "POST", "/transform/submit", key, stub)
	if err != nil {
		return err
	}
	var titled struct{ Files []string }
	json.Unmarshal(data, &titled)
	if !slices.Equal(titled.Files, []string{"Study guide.pdf"}) {
		return fmt.Errorf("a result without a name isn't named after the title: %s", data)
	}

	third, key, err := startJob("start a third")
	if err != nil {
		return err
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
