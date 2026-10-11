package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMarkdownToHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello *world*", "<p>Hello <em>world</em></p>\n"},
		{"# Title\n\nOne\ntwo", "<h1>Title</h1>\n<p>One<br>\ntwo</p>\n"},
		{"**bold** and `a*b*c`", "<p><strong>bold</strong> and <code>a*b*c</code></p>\n"},
		{"- a\n- b\n  - c\n", "<ul>\n<li>a</li>\n<li>b\n<ul>\n<li>c</li>\n</ul>\n</li>\n</ul>\n"},
		{"3. x\n4. y", "<ol start=\"3\">\n<li>x</li>\n<li>y</li>\n</ol>\n"},
		{"[my_file_name](https://x.org/a_b_c)", "<p><a href=\"https://x.org/a_b_c\">my_file_name</a></p>\n"},
		{"a < b & c", "<p>a &lt; b &amp; c</p>\n"},
		{`<p style="text-align:center">Centered</p>`, "<p style=\"text-align:center\">Centered</p>\n"},
		{"> quoted", "<blockquote>\n<p>quoted</p>\n</blockquote>\n"},
		{"| A | B |\n|---|---|\n| 1 | 2 |", "<table>\n<thead><tr><th>A</th><th>B</th></tr></thead>\n<tbody>\n<tr><td>1</td><td>2</td></tr>\n</tbody>\n</table>\n"},
		{"snake_case_word stays", "<p>snake_case_word stays</p>\n"},
		{"---", "<hr>\n"},
		{"```\n<b>x</b>\n```", "<pre><code>&lt;b&gt;x&lt;/b&gt;</code></pre>\n"},
	}
	for _, c := range cases {
		if got := markdownToHTML(c.in); got != c.want {
			t.Errorf("markdownToHTML(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<p>Read <strong>chapter 2 </strong>and see <a href="/courses/1/files/42?wrap=1">the handout</a>.</p>
<ul><li>One</li><li>Two</li></ul><p><img src="/x.png" alt="diagram"></p><script>bad()</script>`
	text, links := htmlToText(in, "https://canvas.example.edu")
	for _, want := range []string{
		"Read **chapter 2** and see [the handout](https://canvas.example.edu/courses/1/files/42?wrap=1).",
		"- One\n- Two",
		"![diagram](https://canvas.example.edu/x.png)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("htmlToText output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "bad()") {
		t.Errorf("script content leaked: %s", text)
	}
	if len(links) != 2 || !filePattern.MatchString(links[0].href) {
		t.Errorf("unexpected links %+v", links)
	}
	// Markdown written by the agent survives a round trip through Canvas.
	md := "## Plan\n\n**Bold** and *italic*, plus [a link](https://example.com).\n\n- first\n- second"
	back, _ := htmlToText(markdownToHTML(md), "")
	if back != md {
		t.Errorf("round trip:\n got %q\nwant %q", back, md)
	}
}

func TestParseOpts(t *testing.T) {
	o, err := parseOpts([]string{"123", "--file", "a.pdf", "456", "--file=b.pdf", "--dry-run", "-7", "--json"}, "file", "dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.pos, []string{"123", "456", "-7"}) || !reflect.DeepEqual(o.vals["file"], []string{"a.pdf", "b.pdf"}) || !o.bools["dry-run"] || !o.bools["json"] {
		t.Errorf("parsed %+v", o)
	}
	if _, err := parseOpts([]string{"--nope"}, "", ""); err == nil {
		t.Error("unknown flag accepted")
	}
}

func TestCourseCurrent(t *testing.T) {
	now := semesterOf(time.Now())
	year := now / 10
	cur := map[string]any{"name": "X", "term": map[string]any{"name": strings.Replace("YYYY Fall C", "YYYY", itoa(year+1), 1)}}
	old := map[string]any{"name": "X", "term": map[string]any{"name": itoa(year-1) + " Spring B"}}
	org := map[string]any{"name": "Club", "course_code": "ORG-" + itoa(year-2) + "Spring-Advising", "term": map[string]any{"name": "2024 ORG"}}
	none := map[string]any{"name": "ASU Ready", "term": map[string]any{"name": "TRN"}}
	if !courseCurrent(cur) || courseCurrent(old) || courseCurrent(org) || !courseCurrent(none) {
		t.Errorf("courseCurrent: cur=%v old=%v org=%v none=%v", courseCurrent(cur), courseCurrent(old), courseCurrent(org), courseCurrent(none))
	}
	if courseCurrent(map[string]any{"concluded": true, "name": "Y"}) {
		t.Error("concluded course counted as current")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestQuizAnswerValue(t *testing.T) {
	cases := []struct {
		typ, raw string
		want     any
	}{
		{"multiple_choice_question", "123", int64(123)},
		{"multiple_answers_question", "1, 2,3", []int64{1, 2, 3}},
		{"short_answer_question", " Paris ", "Paris"},
		{"fill_in_multiple_blanks_question", `{"a":"x"}`, map[string]any{"a": "x"}},
		{"essay_question", "Hi *there*", "<p>Hi <em>there</em></p>\n"},
	}
	for _, c := range cases {
		got, err := quizAnswerValue(c.typ, c.raw)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("quizAnswerValue(%s, %q) = %#v, %v; want %#v", c.typ, c.raw, got, err, c.want)
		}
	}
	if _, err := quizAnswerValue("multiple_choice_question", "Paris"); err == nil {
		t.Error("non-numeric multiple choice answer accepted")
	}
}

// fakeCC stands in for the command center's Canvas relay.
type fakeCC struct {
	*httptest.Server
	assignment map[string]any
	requests   []string
	submitted  url.Values
	posted     url.Values
	postPath   string
}

func newFakeCC(t *testing.T) *fakeCC {
	f := &fakeCC{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("X-Delta-Canvas", "1")
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		switch p := r.URL.Path; {
		case p == "/canvas-status":
			reply(map[string]any{"base_url": "https://canvas.example.edu", "ready": true})
		case p == "/canvas/api/v1/users/self/profile":
			reply(map[string]any{"time_zone": "America/Phoenix"})
		case p == "/canvas/api/v1/courses/1/assignments/2":
			reply(f.assignment)
		case strings.HasPrefix(p, "/canvas-upload/api/v1/courses/1/assignments/2/submissions/self/files"):
			body, _ := io.ReadAll(r.Body)
			if r.URL.Query().Get("name") == "" || len(body) == 0 || r.ContentLength != int64(len(body)) {
				t.Errorf("bad upload request %s (%d bytes)", r.URL, len(body))
			}
			reply(map[string]any{"id": 55, "display_name": r.URL.Query().Get("name")})
		case p == "/canvas/api/v1/courses/1/assignments/2/submissions" && r.Method == "POST":
			r.ParseForm()
			f.submitted = r.PostForm
			reply(map[string]any{"attempt": 1, "workflow_state": "submitted", "submitted_at": "2026-10-11T10:00:00Z"})
		case p == "/canvas/api/v1/courses/1/discussion_topics/9":
			reply(map[string]any{"id": 9, "group_topic_children": []any{map[string]any{"id": 91, "group_id": 7}, map[string]any{"id": 92, "group_id": 8}}})
		case p == "/canvas/api/v1/users/self/groups":
			reply([]any{map[string]any{"id": 8, "name": "Team B"}})
		case p == "/canvas/api/v1/groups/8/discussion_topics/92":
			reply(map[string]any{"id": 92})
		case strings.HasPrefix(p, "/canvas/api/v1/groups/8/discussion_topics/92/entries"):
			r.ParseForm()
			f.posted, f.postPath = r.PostForm, p
			reply(map[string]any{"id": 1234})
		default:
			w.WriteHeader(404)
			reply(map[string]any{"errors": []any{map[string]any{"message": "The specified resource does not exist."}}})
		}
	}))
	t.Setenv("DELTA_SERVER", f.URL)
	t.Setenv("DELTA_TOKEN", "tok")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(f.Close)
	return f
}

func quiet(t *testing.T) {
	old := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = old; devnull.Close() })
}

func TestSubmit(t *testing.T) {
	f := newFakeCC(t)
	quiet(t)
	f.assignment = map[string]any{
		"id": 2, "name": "Essay", "due_at": "2026-10-20T06:59:00Z",
		"submission_types":   []any{"online_upload", "online_text_entry"},
		"allowed_extensions": []any{"pdf"},
		"submission":         map[string]any{"attempt": nil, "workflow_state": "unsubmitted"},
	}
	dir := t.TempDir()
	pdf, docx := dir+"/work.pdf", dir+"/work.docx"
	os.WriteFile(pdf, []byte("%PDF-1.7 fake"), 0o600)
	os.WriteFile(docx, []byte("PK fake"), 0o600)

	if err := run([]string{"canvas", "submit", "1", "2", "--file", docx}); err == nil || !strings.Contains(err.Error(), "only accepts pdf") {
		t.Errorf("docx should be rejected, got %v", err)
	}
	if err := run([]string{"canvas", "submit", "1", "2", "--url", "https://x.org"}); err == nil || !strings.Contains(err.Error(), "does not accept online_url") {
		t.Errorf("url should be rejected, got %v", err)
	}
	if err := run([]string{"canvas", "submit", "1", "2", "--file", pdf, "--dry-run"}); err != nil || f.submitted != nil {
		t.Errorf("dry run: err %v, submitted %v", err, f.submitted)
	}
	if err := run([]string{"canvas", "submit", "1", "2", "--file", pdf, "--comment", "Thanks!"}); err != nil {
		t.Fatal(err)
	}
	want := url.Values{"submission[submission_type]": {"online_upload"}, "submission[file_ids][]": {"55"}, "comment[text_comment]": {"Thanks!"}}
	if !reflect.DeepEqual(f.submitted, want) {
		t.Errorf("submitted %v, want %v", f.submitted, want)
	}
	if err := run([]string{"canvas", "submit", "1", "2", "--text", "# Essay\n\nBody"}); err != nil {
		t.Fatal(err)
	}
	if f.submitted.Get("submission[submission_type]") != "online_text_entry" || f.submitted.Get("submission[body]") != "<h1>Essay</h1>\n<p>Body</p>\n" {
		t.Errorf("text submission %v", f.submitted)
	}
	f.assignment["locked_for_user"] = true
	if err := run([]string{"canvas", "submit", "1", "2", "--file", pdf}); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("locked assignment accepted: %v", err)
	}
}

func TestPostUsesGroupTopic(t *testing.T) {
	f := newFakeCC(t)
	quiet(t)
	if err := run([]string{"canvas", "post", "1", "9", "--reply-to", "77", "--text", "I agree **strongly**."}); err != nil {
		t.Fatal(err)
	}
	if f.postPath != "/canvas/api/v1/groups/8/discussion_topics/92/entries/77/replies" {
		t.Errorf("posted to %s", f.postPath)
	}
	if got := f.posted.Get("message"); got != "<p>I agree <strong>strongly</strong>.</p>\n" {
		t.Errorf("message %q", got)
	}
}

func TestCanvasErrors(t *testing.T) {
	newFakeCC(t)
	quiet(t)
	err := run([]string{"canvas", "assignment", "1", "404"})
	if err == nil || !strings.Contains(err.Error(), "Canvas: The specified resource does not exist. (HTTP 404") {
		t.Errorf("got %v", err)
	}
}
