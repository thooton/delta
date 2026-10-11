package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Classic Quizzes can be taken through the API: start an attempt, answer
// questions (saved as you go), then turn it in. New Quizzes (an LTI tool)
// have no student API and appear as assignments with is_quiz_lti_assignment.

func quizArgs(c *canvasClient, o *opts, usage string) (string, string, error) {
	if err := o.need(2, 2, usage); err != nil {
		return "", "", err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return "", "", err
	}
	qid, err := needID("quiz", o.pos[1])
	return course, qid, err
}

func cmdQuizzes(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "quizzes <course>"); err != nil {
		return err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	list, err := c.getAll("courses/"+course+"/quizzes", nil)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("No Classic Quizzes.")
	} else {
		tw := newTable()
		fmt.Fprintln(tw, "ID\tDUE\tQUESTIONS\tTIME LIMIT\tTYPE\tTITLE")
		for _, q := range list {
			limit := "none"
			if l := gs(q, "time_limit"); l != "" {
				limit = l + " min"
			}
			title := gs(q, "title")
			if gb(q, "locked_for_user") {
				title += " (locked)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", gs(q, "id"), c.fmtTime(gs(q, "due_at")), gs(q, "question_count"), limit, gs(q, "quiz_type"), title)
		}
		tw.Flush()
	}
	fmt.Printf("\nNew Quizzes are not listed here; they show up in `delta canvas assignments %s` as \"new quiz\" and must be taken in a browser.\n", course)
	return nil
}

func cmdQuiz(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	course, qid, err := quizArgs(c, o, "quiz <course> <quiz-id>")
	if err != nil {
		return err
	}
	q, err := c.getObj("courses/"+course+"/quizzes/"+qid, nil)
	if err != nil {
		return err
	}
	subs, _ := c.getAll("courses/"+course+"/quizzes/"+qid+"/submissions", nil)
	if o.bools["json"] {
		q["my_submissions"] = subs
		return printJSON(q)
	}
	fmt.Printf("%s (quiz %s, course %s)\n", gs(q, "title"), qid, course)
	fmt.Printf("Type:         %s, %s question(s), %s pts\n", gs(q, "quiz_type"), gs(q, "question_count"), pts(q, "points_possible"))
	fmt.Printf("Due:          %s\n", c.fmtTime(gs(q, "due_at")))
	if s := gs(q, "unlock_at"); s != "" {
		fmt.Printf("Available:    %s\n", c.fmtTime(s))
	}
	if s := gs(q, "lock_at"); s != "" {
		fmt.Printf("Closes:       %s\n", c.fmtTime(s))
	}
	limit := "none"
	if l := gs(q, "time_limit"); l != "" {
		limit = l + " minutes (the clock starts at quiz-start and keeps running)"
	}
	fmt.Printf("Time limit:   %s\n", limit)
	attempts := gs(q, "allowed_attempts")
	if attempts == "-1" {
		attempts = "unlimited"
	}
	fmt.Printf("Attempts:     %s allowed (scoring: %s)\n", attempts, firstNonEmpty(gs(q, "scoring_policy"), "keep_highest"))
	if gb(q, "one_question_at_a_time") {
		note := "one question at a time"
		if gb(q, "cant_go_back") {
			note += ", and answers are locked once you move on"
		}
		fmt.Printf("Format:       %s\n", note)
	}
	if gb(q, "has_access_code") {
		fmt.Println("Access code:  required (ask the user; pass --access-code)")
	}
	if gb(q, "locked_for_user") {
		fmt.Printf("LOCKED:       %s\n", firstNonEmpty(gs(q, "lock_explanation"), "not available"))
	}
	if gs(q, "ip_filter") != "" || gb(q, "require_lockdown_browser") {
		fmt.Println("Restrictions: this quiz is restricted to certain networks or a lockdown browser; it may not be possible to take it here.")
	}
	c.printHTML("Instructions", gs(q, "description"))
	if len(subs) > 0 {
		fmt.Println("\nYour attempts:")
		for _, s := range subs {
			fmt.Printf("  attempt %s: %s", gs(s, "attempt"), gs(s, "workflow_state"))
			if gs(s, "score") != "" {
				fmt.Printf(", score %s/%s", pts(s, "score"), pts(s, "quiz_points_possible"))
			}
			if gs(s, "finished_at") != "" {
				fmt.Printf(", finished %s", c.fmtDate(gs(s, "finished_at")))
			} else if gs(s, "end_at") != "" {
				fmt.Printf(", time runs out %s", c.fmtTime(gs(s, "end_at")))
			}
			fmt.Println()
		}
	}
	fmt.Printf("\nTo take it: `delta canvas quiz-start %s %s`. Starting an attempt uses one of your allowed attempts and starts any timer.\n", course, qid)
	return nil
}

// quizTokens persists validation tokens locally in case Canvas omits them
// when an in-progress attempt is fetched again.
func quizTokenPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "delta", "quiz-tokens.json")
}

func saveQuizToken(subID, token string) {
	path := quizTokenPath()
	if path == "" || token == "" {
		return
	}
	tokens := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &tokens)
	}
	tokens[subID] = token
	data, _ := json.Marshal(tokens)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, data, 0o600)
}

func loadQuizToken(subID string) string {
	tokens := map[string]string{}
	if data, err := os.ReadFile(quizTokenPath()); err == nil {
		json.Unmarshal(data, &tokens)
	}
	return tokens[subID]
}

// currentAttempt returns the user's in-progress quiz submission.
func (c *canvasClient) currentAttempt(course, qid string) (map[string]any, error) {
	v, err := c.get("courses/"+course+"/quizzes/"+qid+"/submission", nil)
	if err != nil {
		return nil, err
	}
	for _, s := range gl(v, "quiz_submissions") {
		if gs(s, "workflow_state") == "untaken" {
			m := s.(map[string]any)
			if gs(m, "validation_token") == "" {
				m["validation_token"] = loadQuizToken(gs(m, "id"))
			} else {
				saveQuizToken(gs(m, "id"), gs(m, "validation_token"))
			}
			return m, nil
		}
	}
	return nil, fmt.Errorf("no quiz attempt is in progress; start one with `delta canvas quiz-start %s %s`", course, qid)
}

func cmdQuizStart(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "access-code", "")
	if err != nil {
		return err
	}
	course, qid, err := quizArgs(c, o, "quiz-start <course> <quiz-id> [--access-code CODE]")
	if err != nil {
		return err
	}
	form := url.Values{}
	if code := o.str("access-code"); code != "" {
		form.Set("access_code", code)
	}
	var sub map[string]any
	v, err := c.send(http.MethodPost, "courses/"+course+"/quizzes/"+qid+"/submissions", form)
	if err != nil {
		// 409 means an attempt is already running; resume it.
		if !strings.Contains(err.Error(), "HTTP 409") {
			return err
		}
		if sub, err = c.currentAttempt(course, qid); err != nil {
			return err
		}
		fmt.Println("Resuming the attempt already in progress.")
	} else {
		if l := gl(v, "quiz_submissions"); len(l) > 0 {
			sub, _ = l[0].(map[string]any)
		}
		if sub == nil {
			return errors.New("Canvas did not return a quiz submission")
		}
		saveQuizToken(gs(sub, "id"), gs(sub, "validation_token"))
		fmt.Printf("Started attempt %s.\n", gs(sub, "attempt"))
	}
	if end := gs(sub, "end_at"); end != "" {
		fmt.Printf("TIME RUNS OUT: %s. Unsaved answers are lost and Canvas submits automatically.\n", c.fmtTime(end))
	}
	return c.printQuizQuestions(course, qid, sub, o.bools["json"])
}

func cmdQuizQuestions(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	course, qid, err := quizArgs(c, o, "quiz-questions <course> <quiz-id>")
	if err != nil {
		return err
	}
	sub, err := c.currentAttempt(course, qid)
	if err != nil {
		return err
	}
	if end := gs(sub, "end_at"); end != "" {
		fmt.Printf("Time runs out %s.\n", c.fmtTime(end))
	}
	return c.printQuizQuestions(course, qid, sub, o.bools["json"])
}

func (c *canvasClient) quizQuestions(sub map[string]any) ([]any, error) {
	v, err := c.get("quiz_submissions/"+gs(sub, "id")+"/questions", nil)
	if err != nil {
		return nil, err
	}
	return gl(v, "quiz_submission_questions"), nil
}

var answerHelp = map[string]string{
	"multiple_choice_question":         "give the answer ID",
	"true_false_question":              "give the answer ID",
	"multiple_answers_question":        "give all chosen answer IDs, comma-separated (e.g. 12,15)",
	"short_answer_question":            "give the text",
	"essay_question":                   "give the text (Markdown is allowed)",
	"numerical_question":               "give a number",
	"calculated_question":              "give a number",
	"fill_in_multiple_blanks_question": `give JSON {"blank": "text", ...}`,
	"multiple_dropdowns_question":      `give JSON {"blank": answer_id, ...}`,
	"matching_question":                `give JSON [{"answer_id": ID, "match_id": ID}, ...]`,
	"file_upload_question":             "not supported here; needs a browser",
	"text_only_question":               "no answer needed",
}

func (c *canvasClient) printQuizQuestions(course, qid string, sub map[string]any, asJSON bool) error {
	qs, err := c.quizQuestions(sub)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(map[string]any{"quiz_submission": sub, "questions": qs})
	}
	for _, q := range qs {
		typ := gs(q, "question_type")
		flag := ""
		if gb(q, "flagged") {
			flag = " [flagged]"
		}
		fmt.Printf("\n--- Question %s (id %s, %s)%s\n", gs(q, "position"), gs(q, "id"), strings.TrimSuffix(typ, "_question"), flag)
		text, links := htmlToText(gs(q, "question_text"), c.base)
		fmt.Println(indent(text, "  "))
		printFileLinks(links)
		blanks := map[string]bool{}
		for _, a := range gl(q, "answers") {
			label := gs(a, "text")
			if label == "" {
				label, _ = htmlToText(gs(a, "html"), c.base)
			}
			prefix := ""
			if b := gs(a, "blank_id"); b != "" {
				prefix = "blank " + b + ": "
				blanks[b] = true
			}
			if typ == "fill_in_multiple_blanks_question" {
				continue // the answer list would only reveal blank names
			}
			fmt.Printf("    [%s] %s%s\n", gs(a, "id"), prefix, label)
		}
		if typ == "fill_in_multiple_blanks_question" && len(blanks) > 0 {
			var names []string
			for b := range blanks {
				names = append(names, b)
			}
			fmt.Printf("    blanks: %s\n", strings.Join(names, ", "))
		}
		if m := gl(q, "matches"); len(m) > 0 {
			fmt.Println("    match options:")
			for _, x := range m {
				fmt.Printf("      (match_id %s) %s\n", gs(x, "match_id"), gs(x, "text"))
			}
		}
		if ans := walk(q, "answer"); ans != nil {
			b, _ := json.Marshal(ans)
			fmt.Printf("  Saved answer: %s\n", b)
		} else {
			fmt.Printf("  Not answered. To answer, %s.\n", answerHelp[typ])
		}
	}
	fmt.Printf(`
Answer with: delta canvas quiz-answer %s %s <question-id> <answer>
(answers save immediately; they can be changed until the quiz is turned in)
When every question is answered: delta canvas quiz-submit %s %s
`, course, qid, course, qid)
	return nil
}

// quizAnswerValue converts a command-line answer into the JSON shape Canvas
// expects for the question type.
func quizAnswerValue(typ, raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	switch typ {
	case "multiple_choice_question", "true_false_question":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("this question needs an answer ID (a number shown in brackets), got %q", raw)
		}
		return n, nil
	case "multiple_answers_question":
		var ids []int64
		for _, p := range strings.FieldsFunc(strings.Trim(raw, "[]"), func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("this question needs answer IDs separated by commas, got %q", raw)
			}
			ids = append(ids, n)
		}
		return ids, nil
	case "fill_in_multiple_blanks_question", "multiple_dropdowns_question", "matching_question":
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("this question needs a JSON answer (%s): %v", answerHelp[typ], err)
		}
		return v, nil
	case "essay_question":
		return markdownToHTML(raw), nil
	case "file_upload_question":
		return nil, errors.New("file upload questions can't be answered through the API")
	default:
		return raw, nil
	}
}

func cmdQuizAnswer(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "access-code answers-file", "")
	if err != nil {
		return err
	}
	if len(o.pos) < 2 {
		return errors.New("usage: delta canvas quiz-answer <course> <quiz-id> (<question-id> <answer> | --answers-file FILE.json)")
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	qid, err := needID("quiz", o.pos[1])
	if err != nil {
		return err
	}
	answers := map[string]string{}
	if f := o.str("answers-file"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("%s: expected a JSON object of question id -> answer: %w", f, err)
		}
		for k, v := range m {
			if s, ok := v.(string); ok {
				answers[k] = s
			} else {
				b, _ := json.Marshal(v)
				answers[k] = string(b)
			}
		}
	}
	if len(o.pos) == 4 {
		answers[o.pos[2]] = o.pos[3]
	} else if len(o.pos) != 2 || len(answers) == 0 {
		return errors.New("usage: delta canvas quiz-answer <course> <quiz-id> (<question-id> <answer> | --answers-file FILE.json)")
	}
	sub, err := c.currentAttempt(course, qid)
	if err != nil {
		return err
	}
	if gs(sub, "validation_token") == "" {
		return errors.New("the attempt's validation token is unknown (it was started elsewhere); answers can't be saved through the API")
	}
	qs, err := c.quizQuestions(sub)
	if err != nil {
		return err
	}
	types := map[string]string{}
	for _, q := range qs {
		types[gs(q, "id")] = gs(q, "question_type")
	}
	var payload []map[string]any
	for id, raw := range answers {
		typ, ok := types[id]
		if !ok {
			return fmt.Errorf("question %s is not in this quiz attempt", id)
		}
		v, err := quizAnswerValue(typ, raw)
		if err != nil {
			return fmt.Errorf("question %s: %w", id, err)
		}
		n, _ := strconv.ParseInt(id, 10, 64)
		payload = append(payload, map[string]any{"id": n, "answer": v})
	}
	attempt, _ := strconv.Atoi(gs(sub, "attempt"))
	body := map[string]any{"attempt": attempt, "validation_token": gs(sub, "validation_token"), "quiz_questions": payload}
	if code := o.str("access-code"); code != "" {
		body["access_code"] = code
	}
	data, _ := json.Marshal(body)
	if _, err := c.call(http.MethodPost, "quiz_submissions/"+gs(sub, "id")+"/questions", nil, "application/json", strings.NewReader(string(data))); err != nil {
		return err
	}
	fmt.Printf("Saved %d answer(s). Not turned in yet.\n", len(payload))
	if end, ok := c.parseTime(gs(sub, "end_at")); ok {
		fmt.Printf("Time left: %s.\n", strings.TrimPrefix(relTime(time.Until(end)), "in "))
	}
	return nil
}

func cmdQuizSubmit(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "access-code", "")
	if err != nil {
		return err
	}
	course, qid, err := quizArgs(c, o, "quiz-submit <course> <quiz-id> [--access-code CODE]")
	if err != nil {
		return err
	}
	sub, err := c.currentAttempt(course, qid)
	if err != nil {
		return err
	}
	if gs(sub, "validation_token") == "" {
		return errors.New("the attempt's validation token is unknown (it was started elsewhere); it can't be turned in through the API")
	}
	qs, err := c.quizQuestions(sub)
	if err == nil {
		var unanswered []string
		for _, q := range qs {
			if walk(q, "answer") == nil && gs(q, "question_type") != "text_only_question" {
				unanswered = append(unanswered, gs(q, "id"))
			}
		}
		if len(unanswered) > 0 {
			fmt.Printf("Warning: %d question(s) unanswered: %s\n", len(unanswered), strings.Join(unanswered, ", "))
		}
	}
	form := url.Values{"attempt": {gs(sub, "attempt")}, "validation_token": {gs(sub, "validation_token")}}
	if code := o.str("access-code"); code != "" {
		form.Set("access_code", code)
	}
	v, err := c.send(http.MethodPost, fmt.Sprintf("courses/%s/quizzes/%s/submissions/%s/complete", course, qid, gs(sub, "id")), form)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(v)
	}
	res := sub
	if l := gl(v, "quiz_submissions"); len(l) > 0 {
		res, _ = l[0].(map[string]any)
	}
	fmt.Printf("TURNED IN: attempt %s, status %s", gs(res, "attempt"), gs(res, "workflow_state"))
	if gs(res, "score") != "" {
		fmt.Printf(", score %s/%s", pts(res, "score"), pts(res, "quiz_points_possible"))
	}
	fmt.Println()
	if gs(res, "workflow_state") == "pending_review" {
		fmt.Println("Some questions (such as essays) will be graded by the instructor.")
	}
	return nil
}
