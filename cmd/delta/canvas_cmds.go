package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type canvasCommand func(c *canvasClient, args []string) error

var canvasCommands map[string]canvasCommand

func init() {
	canvasCommands = map[string]canvasCommand{
		"status": cmdStatus, "courses": cmdCourses, "course": cmdCourse,
		"todo": cmdTodo, "missing": cmdMissing, "recent": cmdRecent,
		"assignments": cmdAssignments, "assignment": cmdAssignment,
		"submission": cmdSubmission, "submit": cmdSubmit, "comment": cmdComment,
		"peer-reviews": cmdPeerReviews,
		"download":     cmdDownload, "files": cmdFiles, "upload": cmdUpload,
		"modules": cmdModules, "mark-done": cmdMarkDone, "mark-viewed": cmdMarkViewed,
		"pages": cmdPages, "page": cmdPage,
		"announcements": cmdAnnouncements, "discussions": cmdDiscussions,
		"discussion": cmdDiscussion, "post": cmdPost,
		"quizzes": cmdQuizzes, "quiz": cmdQuiz, "quiz-start": cmdQuizStart,
		"quiz-questions": cmdQuizQuestions, "quiz-answer": cmdQuizAnswer, "quiz-submit": cmdQuizSubmit,
		"grades": cmdGrades, "people": cmdPeople, "groups": cmdGroups,
		"inbox": cmdInbox, "thread": cmdThread, "message": cmdMessage, "reply-message": cmdReplyMessage,
		"open": cmdOpen, "api": cmdAPI,
	}
}

func runCanvas(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(canvasHelp)
		return nil
	}
	cmd, ok := canvasCommands[args[0]]
	if !ok {
		return fmt.Errorf("unknown canvas command %q (run `delta canvas help`)", args[0])
	}
	c, err := newCanvasClient()
	if err != nil {
		return err
	}
	if err := c.init(); err != nil {
		return err
	}
	return cmd(c, args[1:])
}

func newTable() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0) }

func pts(v any, keys ...string) string {
	f, ok := gf(v, keys...)
	if !ok {
		return "-"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// subStatus summarizes a submission object, e.g. "graded 18/20 (A-), late".
func subStatus(sub any, pointsPossible string) string {
	if sub == nil {
		return ""
	}
	var parts []string
	ws := gs(sub, "workflow_state")
	switch {
	case gb(sub, "excused"):
		parts = append(parts, "excused")
	case gs(sub, "score") != "" && (ws == "graded" || gs(sub, "posted_at") != ""):
		s := "graded " + pts(sub, "score")
		if pointsPossible != "" && pointsPossible != "-" {
			s += "/" + pointsPossible
		}
		if g := gs(sub, "grade"); g != "" && !sameNumber(g, gs(sub, "score")) {
			s += " (" + g + ")"
		}
		parts = append(parts, s)
	case ws == "unsubmitted" || ws == "":
		parts = append(parts, "not submitted")
	case ws == "pending_review":
		parts = append(parts, "submitted, pending review")
	default:
		parts = append(parts, ws)
	}
	if gb(sub, "missing") {
		parts = append(parts, "MISSING")
	}
	if gb(sub, "late") {
		parts = append(parts, "late")
	}
	return strings.Join(parts, ", ")
}

// ---- overview ----

func cmdStatus(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	var st map[string]any
	if err := c.ccJSON("/canvas-status", &st); err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(st)
	}
	fmt.Printf("Canvas:          %s\n", gs(st, "base_url"))
	ready := "ready"
	if !gb(st, "ready") {
		ready = "NOT READY: " + firstNonEmpty(gs(st, "error"), gs(st, "cookie_service", "error"))
	}
	fmt.Printf("Session:         %s\n", ready)
	if ts, ok := gf(st, "cookie_service", "last_success"); ok {
		fmt.Printf("Last verified:   %s\n", c.fmtTime(time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)))
	}
	me, err := c.getObj("users/self", nil)
	if err != nil {
		return err
	}
	fmt.Printf("Logged in as:    %s (user %s)\n", gs(me, "name"), gs(me, "id"))
	fmt.Printf("Time zone:       %s (times below are shown in it)\n", c.loc)
	return nil
}

func studentEnrollment(course any) any {
	for _, e := range gl(course, "enrollments") {
		if t := gs(e, "type"); t == "student" || t == "StudentEnrollment" {
			return e
		}
	}
	if l := gl(course, "enrollments"); len(l) > 0 {
		return l[0]
	}
	return nil
}

func enrollmentGrade(e any) string {
	score, grade := gs(e, "computed_current_score"), gs(e, "computed_current_grade")
	if score == "" {
		return "-"
	}
	s := pts(e, "computed_current_score") + "%"
	if grade != "" {
		s += " " + grade
	}
	return s
}

func cmdCourses(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "all")
	if err != nil {
		return err
	}
	courses, err := c.courseList()
	if err != nil {
		return err
	}
	var shown []any
	for _, co := range courses {
		if gs(co, "name") != "" && (o.bools["all"] || courseCurrent(co)) {
			shown = append(shown, co)
		}
	}
	sort.SliceStable(shown, func(i, j int) bool {
		return gs(shown[i], "term", "start_at") > gs(shown[j], "term", "start_at")
	})
	if o.bools["json"] {
		return printJSON(shown)
	}
	if len(shown) == 0 {
		fmt.Println("No current courses. Use --all to include past ones.")
		return nil
	}
	tw := newTable()
	fmt.Fprintln(tw, "ID\tNAME\tTERM\tGRADE")
	for _, co := range shown {
		name := gs(co, "name")
		if !courseCurrent(co) {
			name += " (past)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", gs(co, "id"), name, gs(co, "term", "name"), enrollmentGrade(studentEnrollment(co)))
	}
	tw.Flush()
	if !o.bools["all"] {
		fmt.Println("\nPast courses are hidden; use --all to see them. Courses can be named by ID or by part of their code/name (e.g. \"thf101\").")
	}
	return nil
}

func cmdCourse(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "course <course>"); err != nil {
		return err
	}
	id, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	co, err := c.getObj("courses/"+id, url.Values{"include[]": {"syllabus_body", "term", "teachers", "total_scores", "public_description"}})
	if err != nil {
		return err
	}
	tabs, _ := c.getAll("courses/"+id+"/tabs", nil)
	if o.bools["json"] {
		co["tabs"] = tabs
		return printJSON(co)
	}
	fmt.Printf("%s (course %s)\n", gs(co, "name"), id)
	fmt.Printf("Code:      %s\n", gs(co, "course_code"))
	fmt.Printf("Term:      %s\n", gs(co, "term", "name"))
	if s := gs(co, "start_at"); s != "" {
		fmt.Printf("Dates:     %s to %s\n", c.fmtDate(s), c.fmtDate(gs(co, "end_at")))
	}
	var teachers []string
	for _, t := range gl(co, "teachers") {
		teachers = append(teachers, fmt.Sprintf("%s (user %s)", gs(t, "display_name"), gs(t, "id")))
	}
	if len(teachers) > 0 {
		fmt.Printf("Teachers:  %s\n", strings.Join(teachers, ", "))
	}
	fmt.Printf("Grade:     %s\n", enrollmentGrade(studentEnrollment(co)))
	fmt.Printf("Home page: %s\n", gs(co, "default_view"))
	fmt.Printf("URL:       %s\n", firstNonEmpty(gs(co, "html_url"), c.base+"/courses/"+id))
	var names []string
	for _, t := range tabs {
		if gs(t, "hidden") != "true" {
			names = append(names, gs(t, "label"))
		}
	}
	if len(names) > 0 {
		fmt.Printf("Sections:  %s\n", strings.Join(names, ", "))
		fmt.Println("           (most course content is usually under Modules: `delta canvas modules " + id + "`)")
	}
	c.printHTML("Syllabus", gs(co, "syllabus_body"))
	return nil
}

func plannableLabel(t string) string {
	switch t {
	case "assignment":
		return "assignment"
	case "quiz":
		return "quiz"
	case "discussion_topic":
		return "discussion"
	case "wiki_page":
		return "page"
	case "calendar_event":
		return "event"
	case "planner_note":
		return "note"
	case "assessment_request":
		return "peer review"
	case "announcement":
		return "announcement"
	}
	return strings.ReplaceAll(t, "_", " ")
}

func cmdTodo(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "days past course", "all")
	if err != nil {
		return err
	}
	days, err := o.int("days", 7)
	if err != nil {
		return err
	}
	past, err := o.int("past", 7)
	if err != nil {
		return err
	}
	now := time.Now()
	q := url.Values{
		"start_date": {now.Add(-time.Duration(past) * 24 * time.Hour).UTC().Format(time.RFC3339)},
		"end_date":   {now.Add(time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)},
	}
	for _, arg := range o.vals["course"] {
		id, err := c.resolveCourse(arg)
		if err != nil {
			return err
		}
		q.Add("context_codes[]", "course_"+id)
	}
	items, err := c.getAll("planner/items", q)
	if err != nil {
		return err
	}
	var shown []any
	for _, it := range items {
		t := gs(it, "plannable_type")
		sub := walk(it, "submissions")
		done := gb(it, "planner_override", "marked_complete") || gb(sub, "submitted") || gb(sub, "excused") || (gb(sub, "graded") && !gb(sub, "missing"))
		if !o.bools["all"] && (done || t == "announcement") {
			continue
		}
		// Past items only matter if they are work that still needs doing.
		if when, ok := c.parseTime(gs(it, "plannable_date")); ok && when.Before(now) && !o.bools["all"] {
			if _, submittable := sub.(map[string]any); !submittable && t != "assessment_request" {
				continue
			}
		}
		if m, ok := it.(map[string]any); ok {
			m["done"] = done
		}
		shown = append(shown, it)
	}
	if o.bools["json"] {
		return printJSON(shown)
	}
	fmt.Printf("To do from %d day(s) ago through the next %d day(s)", past, days)
	if !o.bools["all"] {
		fmt.Print(" (finished work and announcements hidden; --all shows them)")
	}
	fmt.Println(":")
	if len(shown) == 0 {
		fmt.Println("  Nothing.")
		return nil
	}
	lastDay := ""
	for _, it := range shown {
		when, _ := c.parseTime(gs(it, "plannable_date"))
		day := when.Format("Mon Jan 2")
		if day != lastDay {
			rel := ""
			if d := int(time.Date(when.Year(), when.Month(), when.Day(), 0, 0, 0, 0, c.loc).Sub(time.Date(now.In(c.loc).Year(), now.In(c.loc).Month(), now.In(c.loc).Day(), 0, 0, 0, 0, c.loc)).Hours() / 24); d == 0 {
				rel = " (today)"
			} else if d == 1 {
				rel = " (tomorrow)"
			} else if d < 0 {
				rel = fmt.Sprintf(" (%d days ago)", -d)
			}
			fmt.Printf("\n%s%s\n", day, rel)
			lastDay = day
		}
		p := walk(it, "plannable")
		title := firstNonEmpty(gs(p, "title"), gs(p, "name"))
		var notes []string
		if pp := pts(p, "points_possible"); pp != "-" {
			notes = append(notes, pp+" pts")
		}
		sub := walk(it, "submissions")
		if s, ok := sub.(map[string]any); ok {
			switch {
			case gb(s, "excused"):
				notes = append(notes, "excused")
			case gb(s, "missing"):
				notes = append(notes, "MISSING")
			case gb(s, "submitted") && gb(s, "graded"):
				notes = append(notes, "graded")
			case gb(s, "submitted"):
				notes = append(notes, "submitted")
			default:
				notes = append(notes, "not submitted")
			}
			if gb(s, "late") {
				notes = append(notes, "late")
			}
			if gb(s, "has_feedback") {
				notes = append(notes, "has feedback")
			}
		}
		if gb(it, "planner_override", "marked_complete") {
			notes = append(notes, "marked done")
		}
		if t := gs(it, "plannable_type"); t == "calendar_event" && gs(p, "location_name") != "" {
			notes = append(notes, "at "+gs(p, "location_name"))
		}
		ref := ""
		if cid := gs(it, "course_id"); cid != "" {
			ref = fmt.Sprintf("  [course %s, %s %s]", cid, plannableLabel(gs(it, "plannable_type")), gs(it, "plannable_id"))
			if gs(it, "plannable_type") == "assessment_request" {
				ref = fmt.Sprintf("  [course %s, %s]", cid, gs(it, "html_url"))
			}
		}
		note := ""
		if len(notes) > 0 {
			note = " (" + strings.Join(notes, ", ") + ")"
		}
		where := ""
		if cn := gs(it, "context_name"); cn != "" {
			where = cn + ": "
		}
		fmt.Printf("  %7s  %-11s %s%s%s%s\n", when.Format("3:04pm"), plannableLabel(gs(it, "plannable_type")), where, strings.TrimSpace(title), note, ref)
	}
	return nil
}

func sameNumber(a, b string) bool {
	x, err1 := strconv.ParseFloat(a, 64)
	y, err2 := strconv.ParseFloat(b, 64)
	return a == b || (err1 == nil && err2 == nil && x == y)
}

func cmdMissing(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "all")
	if err != nil {
		return err
	}
	all, err := c.getAll("users/self/missing_submissions", url.Values{"include[]": {"course", "planner_overrides"}, "filter[]": {"submittable"}})
	if err != nil {
		return err
	}
	var list []any
	hidden := 0
	for _, a := range all {
		if o.bools["all"] || courseCurrent(walk(a, "course")) {
			list = append(list, a)
		} else {
			hidden++
		}
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	if hidden > 0 {
		defer fmt.Printf("\n%d missing item(s) from past courses hidden; --all shows them.\n", hidden)
	}
	if len(list) == 0 {
		fmt.Println("No missing work in current courses.")
		return nil
	}
	fmt.Println("Missing work (past due and not submitted):")
	tw := newTable()
	fmt.Fprintln(tw, "COURSE\tASSIGNMENT\tDUE\tPTS\tNAME")
	for _, a := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s — %s\n", gs(a, "course_id"), gs(a, "id"), c.fmtTime(gs(a, "due_at")), pts(a, "points_possible"), gs(a, "course", "name"), strings.TrimSpace(gs(a, "name")))
	}
	return tw.Flush()
}

func cmdRecent(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "n course", "")
	if err != nil {
		return err
	}
	n, err := o.int("n", 25)
	if err != nil {
		return err
	}
	path := "users/self/activity_stream"
	if arg := o.str("course"); arg != "" {
		id, err := c.resolveCourse(arg)
		if err != nil {
			return err
		}
		path = "courses/" + id + "/activity_stream"
	}
	v, err := c.get(path, url.Values{"per_page": {strconv.Itoa(n)}})
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(v)
	}
	items, _ := v.([]any)
	if len(items) == 0 {
		fmt.Println("No recent activity.")
		return nil
	}
	fmt.Println("Recent activity, newest first (grades, comments, announcements, messages, discussions):")
	for _, it := range items {
		typ := gs(it, "type")
		title := gs(it, "title")
		ref := ""
		switch typ {
		case "Submission":
			title = firstNonEmpty(gs(it, "assignment", "name"), title)
			status := subStatus(it, pts(it, "assignment", "points_possible"))
			if n := len(gl(it, "submission_comments")); n > 0 {
				status += fmt.Sprintf(", %d comment(s)", n)
			}
			title += " — " + status
			ref = fmt.Sprintf("[course %s, assignment %s]", gs(it, "course_id"), gs(it, "assignment_id"))
		case "Announcement", "DiscussionTopic":
			ref = fmt.Sprintf("[course %s, topic %s]", gs(it, "course_id"), firstNonEmpty(gs(it, "announcement_id"), gs(it, "discussion_topic_id")))
		case "Conversation":
			ref = fmt.Sprintf("[conversation %s]", gs(it, "conversation_id"))
		default:
			if id := gs(it, "course_id"); id != "" {
				ref = "[course " + id + "]"
			}
		}
		unread := ""
		if gs(it, "read_state") == "false" {
			unread = " (unread)"
		}
		fmt.Printf("- %s  %s: %s%s %s\n", c.fmtDate(gs(it, "updated_at")), typ, title, unread, ref)
		if typ != "Submission" {
			if text, _ := htmlToText(gs(it, "message"), c.base); text != "" {
				fmt.Println(indent(truncate(text, 300), "    "))
			}
		}
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "… [truncated]"
}

// ---- assignments and submissions ----

func cmdAssignments(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "bucket search", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "assignments <course> [--bucket upcoming|overdue|past|undated|unsubmitted|future] [--search TEXT]"); err != nil {
		return err
	}
	id, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	q := url.Values{"include[]": {"submission"}, "order_by": {"due_at"}}
	if b := o.str("bucket"); b != "" {
		q.Set("bucket", b)
	}
	if s := o.str("search"); s != "" {
		q.Set("search_term", s)
	}
	list, err := c.getAll("courses/"+id+"/assignments", q)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	fmt.Printf("Assignments in %s:\n", c.courseName(id))
	if len(list) == 0 {
		fmt.Println("  None.")
		return nil
	}
	tw := newTable()
	fmt.Fprintln(tw, "ID\tDUE\tPTS\tSTATUS\tSUBMIT AS\tNAME")
	for _, a := range list {
		due := "no due date"
		if d := gs(a, "due_at"); d != "" {
			due = c.fmtTime(d)
		}
		name := strings.TrimSpace(gs(a, "name"))
		if gb(a, "locked_for_user") {
			name += " (locked)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", gs(a, "id"), due, pts(a, "points_possible"), subStatus(walk(a, "submission"), pts(a, "points_possible")), submitAs(a), name)
	}
	return tw.Flush()
}

func submitAs(a any) string {
	var types []string
	for _, t := range gl(a, "submission_types") {
		s := gs(t)
		switch s {
		case "online_upload":
			s = "file"
		case "online_text_entry":
			s = "text"
		case "online_url":
			s = "url"
		case "online_quiz":
			s = "quiz"
		case "discussion_topic":
			s = "discussion"
		case "external_tool":
			s = "external tool"
		case "on_paper":
			s = "on paper"
		case "none", "not_graded":
			s = "nothing to submit"
		case "media_recording":
			s = "media"
		case "student_annotation":
			s = "annotation"
		}
		types = append(types, s)
	}
	if gb(a, "is_quiz_lti_assignment") {
		return "new quiz"
	}
	return strings.Join(types, ",")
}

func (c *canvasClient) assignment(course, id string) (map[string]any, error) {
	return c.getObj("courses/"+course+"/assignments/"+id, url.Values{"include[]": {"submission", "can_submit", "overrides"}})
}

func cmdAssignment(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "assignment <course> <assignment-id>"); err != nil {
		return err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	aid, err := needID("assignment", o.pos[1])
	if err != nil {
		return err
	}
	a, err := c.assignment(course, aid)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(a)
	}
	pp := pts(a, "points_possible")
	fmt.Printf("%s (assignment %s in %s, course %s)\n", gs(a, "name"), aid, c.courseName(course), course)
	fmt.Printf("URL:          %s\n", gs(a, "html_url"))
	due := "no due date"
	if d := gs(a, "due_at"); d != "" {
		due = c.fmtTime(d)
	}
	fmt.Printf("Due:          %s\n", due)
	if d := gs(a, "unlock_at"); d != "" {
		fmt.Printf("Available:    from %s\n", c.fmtTime(d))
	}
	if d := gs(a, "lock_at"); d != "" {
		fmt.Printf("Closes:       %s (no submissions after this)\n", c.fmtTime(d))
	}
	fmt.Printf("Points:       %s (%s)\n", pp, gs(a, "grading_type"))
	fmt.Printf("Submit as:    %s\n", strings.Join(stringsOf(gl(a, "submission_types")), ", "))
	if ext := stringsOf(gl(a, "allowed_extensions")); len(ext) > 0 {
		fmt.Printf("File types:   only %s\n", strings.Join(ext, ", "))
	}
	if n, ok := gf(a, "allowed_attempts"); ok && n > 0 {
		fmt.Printf("Attempts:     %s allowed\n", pts(a, "allowed_attempts"))
	}
	if gs(a, "group_category_id") != "" {
		fmt.Println("Group work:   yes (one submission counts for the whole group)")
	}
	if gb(a, "peer_reviews") {
		fmt.Println("Peer review:  yes (see `delta canvas peer-reviews " + course + " " + aid + "`)")
	}
	if gb(a, "locked_for_user") {
		text, _ := htmlToText(gs(a, "lock_explanation"), c.base)
		fmt.Printf("LOCKED:       %s\n", firstNonEmpty(text, "this assignment is locked"))
	}
	switch {
	case gb(a, "is_quiz_lti_assignment"):
		fmt.Println("Note:         This is a New Quiz. Canvas has no student API for New Quizzes, so it cannot be taken through delta; the user must open it in a browser.")
	case gs(a, "quiz_id") != "":
		fmt.Printf("Note:         This is a quiz; see `delta canvas quiz %s %s`.\n", course, gs(a, "quiz_id"))
	case walk(a, "discussion_topic") != nil:
		fmt.Printf("Note:         Submit by posting in the discussion: `delta canvas discussion %s %s`, then `delta canvas post ...`.\n", course, gs(a, "discussion_topic", "id"))
	case contains(gl(a, "submission_types"), "external_tool"):
		fmt.Println("Note:         Submitted through an external tool; this usually needs a browser.")
	}
	c.printHTML("Instructions", gs(a, "description"))
	if rubric := gl(a, "rubric"); len(rubric) > 0 {
		fmt.Println("\nRubric:")
		for _, cr := range rubric {
			fmt.Printf("  - %s (%s pts) [criterion %s]\n", gs(cr, "description"), pts(cr, "points"), gs(cr, "id"))
			if ld, _ := htmlToText(gs(cr, "long_description"), c.base); ld != "" {
				fmt.Println(indent(ld, "      "))
			}
			for _, r := range gl(cr, "ratings") {
				fmt.Printf("      %s pts: %s %s\n", pts(r, "points"), gs(r, "description"), gs(r, "long_description"))
			}
		}
	}
	sub := walk(a, "submission")
	fmt.Printf("\nYour submission: %s", firstNonEmpty(subStatus(sub, pp), "unknown"))
	if s := gs(sub, "submitted_at"); s != "" {
		fmt.Printf(", submitted %s, attempt %s", c.fmtTime(s), gs(sub, "attempt"))
	}
	fmt.Printf("\nDetails, files and feedback: `delta canvas submission %s %s`\n", course, aid)
	return nil
}

func stringsOf(l []any) []string {
	var out []string
	for _, v := range l {
		out = append(out, gs(v))
	}
	return out
}

func contains(l []any, s string) bool {
	for _, v := range l {
		if gs(v) == s {
			return true
		}
	}
	return false
}

func cmdSubmission(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "user", "")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "submission <course> <assignment-id> [--user USER_ID]"); err != nil {
		return err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	aid, err := needID("assignment", o.pos[1])
	if err != nil {
		return err
	}
	user := firstNonEmpty(o.str("user"), "self")
	sub, err := c.getObj(fmt.Sprintf("courses/%s/assignments/%s/submissions/%s", course, aid, user),
		url.Values{"include[]": {"submission_comments", "rubric_assessment", "submission_history", "assignment"}})
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(sub)
	}
	a := walk(sub, "assignment")
	pp := pts(a, "points_possible")
	fmt.Printf("Submission for %s (assignment %s, course %s)\n", firstNonEmpty(gs(a, "name"), "assignment"), aid, course)
	fmt.Printf("Status:     %s\n", subStatus(sub, pp))
	if s := gs(sub, "submitted_at"); s != "" {
		fmt.Printf("Submitted:  %s (attempt %s, as %s)\n", c.fmtTime(s), gs(sub, "attempt"), gs(sub, "submission_type"))
	}
	if s := gs(sub, "graded_at"); s != "" {
		fmt.Printf("Graded:     %s\n", c.fmtTime(s))
	}
	if s := gs(sub, "url"); s != "" {
		fmt.Printf("URL:        %s\n", s)
	}
	if s := gs(sub, "preview_url"); s != "" {
		fmt.Printf("Preview:    %s\n", s)
	}
	c.printHTML("Text submitted", gs(sub, "body"))
	printAttachments("Files submitted", gl(sub, "attachments"))
	if ra, ok := walk(sub, "rubric_assessment").(map[string]any); ok && len(ra) > 0 {
		fmt.Println("\nRubric scores:")
		names := map[string]string{}
		for _, cr := range gl(a, "rubric") {
			names[gs(cr, "id")] = gs(cr, "description")
		}
		for _, k := range sortedKeys(ra) {
			fmt.Printf("  - %s: %s pts", firstNonEmpty(names[k], k), pts(ra[k], "points"))
			if cm := gs(ra[k], "comments"); cm != "" {
				fmt.Printf(" — %s", cm)
			}
			fmt.Println()
		}
	}
	comments := gl(sub, "submission_comments")
	if len(comments) > 0 {
		fmt.Println("\nComments:")
		for _, cm := range comments {
			fmt.Printf("  %s, %s:\n", gs(cm, "author_name"), c.fmtDate(gs(cm, "created_at")))
			fmt.Println(indent(gs(cm, "comment"), "    "))
			printAttachments("    Attached", gl(cm, "attachments"))
		}
	}
	if hist := gl(sub, "submission_history"); len(hist) > 1 {
		fmt.Println("\nAttempts:")
		for _, h := range hist {
			if gs(h, "submitted_at") == "" {
				continue
			}
			fmt.Printf("  attempt %s: %s, %s\n", gs(h, "attempt"), c.fmtDate(gs(h, "submitted_at")), subStatus(h, pp))
		}
	}
	return nil
}

func printAttachments(heading string, atts []any) {
	if len(atts) == 0 {
		return
	}
	pad := heading[:len(heading)-len(strings.TrimLeft(heading, " "))]
	fmt.Printf("\n%s:\n", heading)
	for _, f := range atts {
		size, _ := gf(f, "size")
		fmt.Printf("%s  - %s (%s, file %s) — download: `delta canvas download %s`\n", pad, firstNonEmpty(gs(f, "display_name"), gs(f, "filename")), humanSize(int64(size)), gs(f, "id"), gs(f, "id"))
	}
}

func cmdSubmit(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "file text text-file html url comment", "dry-run")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "submit <course> <assignment-id> (--file PATH... | --text TEXT | --text-file PATH | --html HTML | --url URL) [--comment TEXT] [--dry-run]"); err != nil {
		return err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	aid, err := needID("assignment", o.pos[1])
	if err != nil {
		return err
	}
	body, err := bodyFromOpts(o, false)
	if err != nil {
		return err
	}
	files, link := o.vals["file"], o.str("url")
	var kind string
	n := 0
	if len(files) > 0 {
		kind, n = "online_upload", n+1
	}
	if body != "" {
		kind, n = "online_text_entry", n+1
	}
	if link != "" {
		kind, n = "online_url", n+1
	}
	if n != 1 {
		return errors.New("give exactly one kind of submission: --file (repeatable), --text/--text-file/--html, or --url")
	}

	a, err := c.assignment(course, aid)
	if err != nil {
		return err
	}
	types := gl(a, "submission_types")
	if !contains(types, kind) {
		hint := ""
		switch {
		case gb(a, "is_quiz_lti_assignment"):
			hint = " It is a New Quiz, which must be taken in a browser."
		case contains(types, "online_quiz"):
			hint = " It is a quiz; use the quiz commands."
		case contains(types, "discussion_topic"):
			hint = " It is a discussion; use `delta canvas post`."
		}
		return fmt.Errorf("%q does not accept %s submissions; it accepts: %s.%s", gs(a, "name"), kind, strings.Join(stringsOf(types), ", "), hint)
	}
	if gb(a, "locked_for_user") {
		text, _ := htmlToText(gs(a, "lock_explanation"), c.base)
		return fmt.Errorf("%q is locked: %s", gs(a, "name"), firstNonEmpty(text, "submissions are closed"))
	}
	if allowed := stringsOf(gl(a, "allowed_extensions")); len(allowed) > 0 {
		for _, f := range files {
			ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(f)), ".")
			ok := false
			for _, x := range allowed {
				ok = ok || strings.ToLower(strings.TrimPrefix(x, ".")) == ext
			}
			if !ok {
				return fmt.Errorf("%s: this assignment only accepts %s files", f, strings.Join(allowed, ", "))
			}
		}
	}
	for _, f := range files {
		if info, err := os.Stat(f); err != nil {
			return err
		} else if info.Size() == 0 {
			return fmt.Errorf("%s is empty", f)
		}
	}
	if max, ok := gf(a, "allowed_attempts"); ok && max > 0 {
		if used, _ := gf(a, "submission", "attempt"); used >= max {
			return fmt.Errorf("all %v allowed attempts have been used", max)
		}
	}

	fmt.Printf("Submitting to %q (%s), due %s\n", gs(a, "name"), c.courseName(course), c.fmtTime(gs(a, "due_at")))
	switch kind {
	case "online_upload":
		for _, f := range files {
			info, _ := os.Stat(f)
			fmt.Printf("  file: %s (%s, %s)\n", f, humanSize(info.Size()), contentTypeFor(f))
		}
	case "online_text_entry":
		text, _ := htmlToText(body, "")
		fmt.Printf("  text entry (%d words):\n%s\n", len(strings.Fields(text)), indent(truncate(text, 600), "    "))
	case "online_url":
		fmt.Printf("  url: %s\n", link)
	}
	if cm := o.str("comment"); cm != "" {
		fmt.Printf("  comment: %s\n", cm)
	}
	if prev := walk(a, "submission"); gs(prev, "submitted_at") != "" {
		fmt.Printf("  note: this will be a new attempt; attempt %s was submitted %s\n", gs(prev, "attempt"), c.fmtTime(gs(prev, "submitted_at")))
	}
	if o.bools["dry-run"] {
		fmt.Println("Dry run: nothing was submitted.")
		return nil
	}

	form := url.Values{"submission[submission_type]": {kind}}
	for _, f := range files {
		file, err := c.upload(fmt.Sprintf("courses/%s/assignments/%s/submissions/self/files", course, aid), f, nil)
		if err != nil {
			return err
		}
		fmt.Printf("  uploaded %s (file %s)\n", gs(file, "display_name"), gs(file, "id"))
		form.Add("submission[file_ids][]", gs(file, "id"))
	}
	if body != "" {
		form.Set("submission[body]", body)
	}
	if link != "" {
		form.Set("submission[url]", link)
	}
	if cm := o.str("comment"); cm != "" {
		form.Set("comment[text_comment]", cm)
	}
	res, err := c.send(http.MethodPost, fmt.Sprintf("courses/%s/assignments/%s/submissions", course, aid), form)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(res)
	}
	fmt.Printf("SUBMITTED: attempt %s at %s, status %q", gs(res, "attempt"), c.fmtTime(gs(res, "submitted_at")), gs(res, "workflow_state"))
	if gb(res, "late") {
		fmt.Print(" (marked late)")
	}
	fmt.Println()
	printAttachments("Files on the submission", gl(res, "attachments"))
	return nil
}

func cmdComment(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "user text text-file file", "")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "comment <course> <assignment-id> --text TEXT [--file PATH]... [--user USER_ID]"); err != nil {
		return err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	aid, err := needID("assignment", o.pos[1])
	if err != nil {
		return err
	}
	text, err := plainFromOpts(o)
	if err != nil {
		return err
	}
	if text == "" && len(o.vals["file"]) == 0 {
		return errors.New("give --text and/or --file")
	}
	user := firstNonEmpty(o.str("user"), "self")
	base := fmt.Sprintf("courses/%s/assignments/%s/submissions/%s", course, aid, user)
	form := url.Values{}
	if text != "" {
		form.Set("comment[text_comment]", text)
	}
	for _, f := range o.vals["file"] {
		file, err := c.upload(base+"/comments/files", f, nil)
		if err != nil {
			return err
		}
		form.Add("comment[file_ids][]", gs(file, "id"))
	}
	res, err := c.send(http.MethodPut, base, form)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(res)
	}
	fmt.Printf("Comment added to the submission for assignment %s (%d comment(s) now).\n", aid, len(gl(res, "submission_comments")))
	return nil
}

// plainFromOpts reads plain text (for comments and inbox messages, which
// Canvas does not render as HTML) from --text or --text-file.
func plainFromOpts(o *opts) (string, error) {
	text := strings.Join(o.vals["text"], "\n\n")
	if p := o.str("text-file"); p != "" {
		if text != "" {
			return "", errors.New("use only one of --text and --text-file")
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		text = string(data)
	}
	return strings.TrimSpace(text), nil
}

func cmdPeerReviews(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "peer-reviews <course> <assignment-id>"); err != nil {
		return err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	aid, err := needID("assignment", o.pos[1])
	if err != nil {
		return err
	}
	list, err := c.getAll(fmt.Sprintf("courses/%s/assignments/%s/peer_reviews", course, aid), url.Values{"include[]": {"user", "submission_comments"}})
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	me, err := c.getObj("users/self", nil)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No peer reviews involve you on this assignment.")
		return nil
	}
	for _, r := range list {
		owner := firstNonEmpty(gs(r, "user", "display_name"), "a classmate")
		if gs(r, "assessor_id") == gs(me, "id") {
			fmt.Printf("- You review %s's work (user %s): %s\n", owner, gs(r, "user_id"), gs(r, "workflow_state"))
			fmt.Printf("    read it:    delta canvas submission %s %s --user %s\n", course, aid, gs(r, "user_id"))
			fmt.Printf("    give feedback: delta canvas comment %s %s --user %s --text \"...\"\n", course, aid, gs(r, "user_id"))
		} else {
			fmt.Printf("- %s reviews your work: %s\n", firstNonEmpty(gs(r, "assessor", "display_name"), "A classmate"), gs(r, "workflow_state"))
		}
	}
	fmt.Println("\nIf the assignment has a rubric, peer reviews may also need the rubric filled in, which requires a browser.")
	return nil
}

// ---- files ----

var contentDispName = regexp.MustCompile(`filename\*?=(?:UTF-8'')?"?([^";]+)"?`)

func cmdDownload(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "out", "to-phone")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "download <file-id | url> [--out PATH] [--to-phone]"); err != nil {
		return err
	}
	target := o.pos[0]
	var name, fetch string
	direct := false
	if m := filePattern.FindStringSubmatch(target); isID(target) || (m != nil && c.isCanvasURL(target)) {
		id := target
		if !isID(target) {
			id = m[1]
		}
		q := url.Values{}
		if u, err := url.Parse(target); err == nil && u.Query().Get("verifier") != "" {
			q.Set("verifier", u.Query().Get("verifier"))
		}
		meta, err := c.getObj("files/"+id, q)
		if err == nil {
			name, fetch = gs(meta, "display_name"), gs(meta, "url")
			if gb(meta, "locked_for_user") {
				return fmt.Errorf("file %s (%s) is locked: %s", id, name, gs(meta, "lock_explanation"))
			}
		} else {
			// Fall back to the browser-style download route (works for
			// files linked from content the user can see).
			fetch = "/files/" + id + "/download?download_frd=1"
			if v := q.Get("verifier"); v != "" {
				fetch += "&verifier=" + url.QueryEscape(v)
			}
		}
	} else if c.isCanvasURL(target) {
		fetch = target
	} else if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		fetch, direct = target, true
	} else {
		return fmt.Errorf("%q is not a file ID or URL", target)
	}

	var resp *http.Response
	if direct || (fetch != "" && !c.isCanvasURL(fetch) && !strings.HasPrefix(fetch, "/")) {
		resp, err = c.http.Get(fetch)
		if err == nil && resp.StatusCode/100 != 2 {
			resp.Body.Close()
			return fmt.Errorf("download failed: HTTP %d from %s", resp.StatusCode, fetch)
		}
	} else {
		resp, err = c.request(http.MethodGet, fetch, nil, "", nil)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if name == "" {
		if m := contentDispName.FindStringSubmatch(resp.Header.Get("Content-Disposition")); m != nil {
			name, _ = url.PathUnescape(m[1])
		}
	}
	if name == "" {
		name = filepath.Base(strings.Split(fetch, "?")[0])
		if ext, _ := mime.ExtensionsByType(strings.Split(resp.Header.Get("Content-Type"), ";")[0]); filepath.Ext(name) == "" && len(ext) > 0 {
			name += ext[0]
		}
	}
	name = strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(name)
	out := name
	if dest := o.str("out"); dest != "" {
		out = dest
		if info, err := os.Stat(dest); err == nil && info.IsDir() {
			out = filepath.Join(dest, name)
		}
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Printf("Saved %s (%s, %s).\n", out, humanSize(n), resp.Header.Get("Content-Type"))
	if o.bools["to-phone"] {
		return push(out, filepath.Base(out))
	}
	return nil
}

func (c *canvasClient) isCanvasURL(s string) bool {
	u, err := url.Parse(s)
	b, _ := url.Parse(c.base)
	return err == nil && b != nil && u.Host == b.Host
}

func cmdUpload(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "to folder", "")
	if err != nil {
		return err
	}
	if err := o.need(1, -1, "upload <path>... [--to me|group:ID] [--folder PATH]"); err != nil {
		return err
	}
	ctx := "users/self"
	if to := o.str("to"); to != "" && to != "me" {
		if ctx, err = c.resolveContext(to); err != nil {
			return err
		}
	}
	extra := url.Values{"on_duplicate": {"rename"}, "parent_folder_path": {firstNonEmpty(o.str("folder"), "uploads")}}
	for _, p := range o.pos {
		f, err := c.upload(ctx+"/files", p, extra)
		if err != nil {
			return err
		}
		if o.bools["json"] {
			printJSON(f)
			continue
		}
		fmt.Printf("Uploaded %s as file %s (%s).\n", p, gs(f, "id"), gs(f, "display_name"))
	}
	return nil
}

func cmdFiles(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "search", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "files <course | group:ID | me> [--search TEXT]"); err != nil {
		return err
	}
	ctx := "users/self"
	if o.pos[0] != "me" {
		if ctx, err = c.resolveContext(o.pos[0]); err != nil {
			return err
		}
	}
	q := url.Values{"sort": {"updated_at"}, "order": {"desc"}}
	if s := o.str("search"); s != "" {
		q.Set("search_term", s)
	}
	list, err := c.getAll(ctx+"/files", q)
	if err != nil {
		return fmt.Errorf("%w\n  Many courses hide the Files section; look for files linked from modules, pages and assignments instead", err)
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("No files.")
		return nil
	}
	tw := newTable()
	fmt.Fprintln(tw, "ID\tSIZE\tUPDATED\tNAME")
	for _, f := range list {
		size, _ := gf(f, "size")
		name := gs(f, "display_name")
		if gb(f, "locked_for_user") {
			name += " (locked)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", gs(f, "id"), humanSize(int64(size)), c.fmtDate(gs(f, "updated_at")), name)
	}
	tw.Flush()
	fmt.Println("\nDownload with `delta canvas download <id>`.")
	return nil
}

// ---- modules and pages ----

func cmdModules(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "search", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "modules <course> [--search TEXT]"); err != nil {
		return err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	q := url.Values{"include[]": {"items", "content_details"}}
	if s := o.str("search"); s != "" {
		q.Set("search_term", s)
	}
	mods, err := c.getAll("courses/"+course+"/modules", q)
	if err != nil {
		return err
	}
	for _, m := range mods {
		// Large modules omit their items from the list response.
		if walk(m, "items") == nil {
			items, err := c.getAll(fmt.Sprintf("courses/%s/modules/%s/items", course, gs(m, "id")), url.Values{"include[]": {"content_details"}})
			if err == nil {
				m.(map[string]any)["items"] = items
			}
		}
	}
	if o.bools["json"] {
		return printJSON(mods)
	}
	if len(mods) == 0 {
		fmt.Println("No modules.")
		return nil
	}
	for _, m := range mods {
		state := gs(m, "state")
		extra := ""
		if s := gs(m, "unlock_at"); s != "" && state == "locked" {
			extra = ", unlocks " + c.fmtTime(s)
		}
		fmt.Printf("\nModule %s: %s [%s%s]\n", gs(m, "id"), gs(m, "name"), firstNonEmpty(state, "no requirements"), extra)
		for _, it := range gl(m, "items") {
			typ := gs(it, "type")
			if typ == "SubHeader" {
				fmt.Printf("  -- %s --\n", gs(it, "title"))
				continue
			}
			ref := ""
			switch typ {
			case "Page":
				ref = "page " + gs(it, "page_url")
			case "ExternalUrl", "ExternalTool":
				ref = gs(it, "external_url")
			default:
				if id := gs(it, "content_id"); id != "" {
					ref = strings.ToLower(typ) + " " + id
				}
			}
			var notes []string
			if d := gs(it, "content_details", "due_at"); d != "" {
				notes = append(notes, "due "+c.fmtTime(d))
			}
			if p := pts(it, "content_details", "points_possible"); p != "-" {
				notes = append(notes, p+" pts")
			}
			if gb(it, "content_details", "locked_for_user") {
				notes = append(notes, "locked")
			}
			if req := walk(it, "completion_requirement"); req != nil {
				done := "todo"
				if gb(req, "completed") {
					done = "done"
				}
				notes = append(notes, fmt.Sprintf("requirement: %s (%s)", strings.ReplaceAll(gs(req, "type"), "_", " "), done))
			}
			note := ""
			if len(notes) > 0 {
				note = " — " + strings.Join(notes, "; ")
			}
			fmt.Printf("  [%s] %s (item %s, %s)%s\n", typ, gs(it, "title"), gs(it, "id"), ref, note)
		}
	}
	fmt.Printf(`
Open items with: delta canvas assignment|quiz|discussion|page %s <id or page url>,
delta canvas download <file id>. Requirements: "must mark done" items use
`+"`delta canvas mark-done %s <module> <item>`"+`; "must view" items for external
content use `+"`delta canvas mark-viewed %s <module> <item>`"+`.
`, course, course, course)
	return nil
}

func moduleItemArgs(c *canvasClient, o *opts, usage string) (string, error) {
	if err := o.need(3, 3, usage); err != nil {
		return "", err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return "", err
	}
	mod, err := needID("module", o.pos[1])
	if err != nil {
		return "", err
	}
	item, err := needID("item", o.pos[2])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("courses/%s/modules/%s/items/%s", course, mod, item), nil
}

func cmdMarkDone(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "undo")
	if err != nil {
		return err
	}
	path, err := moduleItemArgs(c, o, "mark-done <course> <module-id> <item-id> [--undo]")
	if err != nil {
		return err
	}
	method := http.MethodPut
	if o.bools["undo"] {
		method = http.MethodDelete
	}
	if _, err := c.send(method, path+"/done", nil); err != nil {
		return err
	}
	if o.bools["undo"] {
		fmt.Println("Module item marked not done.")
	} else {
		fmt.Println("Module item marked done.")
	}
	return nil
}

func cmdMarkViewed(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	path, err := moduleItemArgs(c, o, "mark-viewed <course> <module-id> <item-id>")
	if err != nil {
		return err
	}
	if _, err := c.send(http.MethodPost, path+"/mark_read", nil); err != nil {
		return err
	}
	fmt.Println("Module item marked viewed.")
	return nil
}

func cmdPages(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "search", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "pages <course | group:ID> [--search TEXT]"); err != nil {
		return err
	}
	ctx, err := c.resolveContext(o.pos[0])
	if err != nil {
		return err
	}
	q := url.Values{"sort": {"title"}}
	if s := o.str("search"); s != "" {
		q.Set("search_term", s)
	}
	list, err := c.getAll(ctx+"/pages", q)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	tw := newTable()
	fmt.Fprintln(tw, "PAGE URL\tUPDATED\tTITLE")
	for _, p := range list {
		title := gs(p, "title")
		if gb(p, "front_page") {
			title += " (front page)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", gs(p, "url"), c.fmtDate(gs(p, "updated_at")), title)
	}
	tw.Flush()
	fmt.Println("\nRead one with `delta canvas page " + o.pos[0] + " <page url>`.")
	return nil
}

func cmdPage(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "page <course | group:ID> <page-url | page-id | front>"); err != nil {
		return err
	}
	ctx, err := c.resolveContext(o.pos[0])
	if err != nil {
		return err
	}
	ref := o.pos[1]
	if m := regexp.MustCompile(`/(?:pages|wiki)/([^/?#]+)`).FindStringSubmatch(ref); m != nil {
		ref, _ = url.PathUnescape(m[1])
	}
	path := ctx + "/pages/" + url.PathEscape(ref)
	if ref == "front" {
		path = ctx + "/front_page"
	}
	p, err := c.getObj(path, nil)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(p)
	}
	fmt.Printf("%s (page %q, updated %s)\n", gs(p, "title"), gs(p, "url"), c.fmtDate(gs(p, "updated_at")))
	if gb(p, "locked_for_user") {
		fmt.Printf("LOCKED: %s\n", gs(p, "lock_explanation"))
	}
	c.printHTML("", gs(p, "body"))
	return nil
}

// ---- announcements and discussions ----

func cmdAnnouncements(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "days", "full")
	if err != nil {
		return err
	}
	days, err := o.int("days", 14)
	if err != nil {
		return err
	}
	q := url.Values{
		"start_date": {time.Now().Add(-time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)},
		"end_date":   {time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)},
	}
	if len(o.pos) > 0 {
		for _, arg := range o.pos {
			id, err := c.resolveCourse(arg)
			if err != nil {
				return err
			}
			q.Add("context_codes[]", "course_"+id)
		}
	} else {
		courses, err := c.courseList()
		if err != nil {
			return err
		}
		for _, co := range courses {
			if courseCurrent(co) {
				q.Add("context_codes[]", "course_"+gs(co, "id"))
			}
		}
	}
	list, err := c.getAll("announcements", q)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Printf("No announcements in the last %d days.\n", days)
		return nil
	}
	for _, a := range list {
		course := strings.TrimPrefix(gs(a, "context_code"), "course_")
		unread := ""
		if gs(a, "read_state") == "unread" {
			unread = " (unread)"
		}
		fmt.Printf("\n== %s%s\n   %s, %s, by %s [course %s, topic %s]\n", gs(a, "title"), unread, c.courseName(course), c.fmtDate(gs(a, "posted_at")), gs(a, "author", "display_name"), course, gs(a, "id"))
		text, links := htmlToText(gs(a, "message"), c.base)
		if !o.bools["full"] {
			text = truncate(text, 800)
		}
		fmt.Println(indent(text, "   "))
		printFileLinks(links)
	}
	fmt.Println("\nUse --full for complete text. Replies: `delta canvas discussion <course> <topic>`.")
	return nil
}

func cmdDiscussions(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "discussions <course | group:ID>"); err != nil {
		return err
	}
	ctx, err := c.resolveContext(o.pos[0])
	if err != nil {
		return err
	}
	list, err := c.getAll(ctx+"/discussion_topics", url.Values{"order_by": {"recent_activity"}})
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("No discussions.")
		return nil
	}
	tw := newTable()
	fmt.Fprintln(tw, "ID\tLAST ACTIVITY\tREPLIES\tUNREAD\tDUE\tTITLE")
	for _, t := range list {
		var flags []string
		if gb(t, "require_initial_post") {
			flags = append(flags, "must post before seeing replies")
		}
		if gb(t, "locked") || gb(t, "locked_for_user") {
			flags = append(flags, "locked")
		}
		if len(gl(t, "group_topic_children")) > 0 {
			flags = append(flags, "group discussion")
		}
		if gs(t, "assignment_id") != "" {
			flags = append(flags, "graded")
		}
		title := gs(t, "title")
		if len(flags) > 0 {
			title += " (" + strings.Join(flags, ", ") + ")"
		}
		due := "-"
		if d := gs(t, "assignment", "due_at"); d != "" {
			due = c.fmtTime(d)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", gs(t, "id"), c.fmtDate(firstNonEmpty(gs(t, "last_reply_at"), gs(t, "posted_at"))), gs(t, "discussion_subentry_count"), gs(t, "unread_count"), due, title)
	}
	return tw.Flush()
}

// topicContext finds where a discussion actually lives. Group discussions
// are split into one copy per group; students read and post in their own.
func (c *canvasClient) topicContext(ctx, topicID string) (string, string, map[string]any, error) {
	topic, err := c.getObj(ctx+"/discussion_topics/"+topicID, nil)
	if err != nil {
		return "", "", nil, err
	}
	children := gl(topic, "group_topic_children")
	if len(children) == 0 || !strings.HasPrefix(ctx, "courses/") {
		return ctx, topicID, topic, nil
	}
	groups, err := c.getAll("users/self/groups", nil)
	if err != nil {
		return "", "", nil, err
	}
	for _, ch := range children {
		for _, g := range groups {
			if gs(ch, "group_id") == gs(g, "id") {
				gctx, gid := "groups/"+gs(g, "id"), gs(ch, "id")
				fmt.Printf("(This is a group discussion; using your group %q: group:%s, topic %s.)\n", gs(g, "name"), gs(g, "id"), gid)
				t, err := c.getObj(gctx+"/discussion_topics/"+gid, nil)
				if err != nil {
					return "", "", nil, err
				}
				return gctx, gid, t, nil
			}
		}
	}
	return ctx, topicID, topic, nil
}

func cmdDiscussion(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "discussion <course | group:ID> <topic-id>"); err != nil {
		return err
	}
	ctx, err := c.resolveContext(o.pos[0])
	if err != nil {
		return err
	}
	tid, err := needID("topic", o.pos[1])
	if err != nil {
		return err
	}
	ctx, tid, topic, err := c.topicContext(ctx, tid)
	if err != nil {
		return err
	}
	view, viewErr := c.getObj(ctx+"/discussion_topics/"+tid+"/view", url.Values{"include_new_entries": {"1"}})
	if o.bools["json"] {
		topic["view"] = view
		return printJSON(topic)
	}
	kind := "Discussion"
	if gb(topic, "is_announcement") {
		kind = "Announcement"
	}
	fmt.Printf("%s: %s (topic %s in %s)\n", kind, gs(topic, "title"), tid, ctx)
	fmt.Printf("Posted %s by %s\n", c.fmtDate(gs(topic, "posted_at")), firstNonEmpty(gs(topic, "author", "display_name"), gs(topic, "user_name")))
	if a := walk(topic, "assignment"); a != nil {
		fmt.Printf("Graded: %s pts, due %s (assignment %s)\n", pts(a, "points_possible"), c.fmtTime(gs(a, "due_at")), gs(a, "id"))
	}
	if gb(topic, "require_initial_post") {
		fmt.Println("You must post before you can see others' replies.")
	}
	if gb(topic, "locked_for_user") {
		fmt.Printf("LOCKED: %s\n", firstNonEmpty(gs(topic, "lock_explanation"), "no new replies allowed"))
	}
	c.printHTML("Prompt", gs(topic, "message"))
	printAttachments("Attached to the prompt", gl(topic, "attachments"))
	if viewErr != nil {
		fmt.Printf("\nReplies unavailable: %v\n", viewErr)
		return nil
	}
	names := map[string]string{}
	for _, p := range gl(view, "participants") {
		names[gs(p, "id")] = gs(p, "display_name")
	}
	entries := gl(view, "view")
	entries = append(entries, gl(view, "new_entries")...)
	fmt.Printf("\nReplies (%d top-level). Reply to one with `delta canvas post %s %s --reply-to <entry> --text ...`:\n", len(gl(view, "view")), o.pos[0], tid)
	if len(entries) == 0 {
		fmt.Println("  None yet.")
	}
	unread := map[string]bool{}
	for _, id := range gl(view, "unread_entries") {
		unread[gs(id)] = true
	}
	var show func(list []any, depth int)
	show = func(list []any, depth int) {
		pad := strings.Repeat("    ", depth+1)
		for _, e := range list {
			if gb(e, "deleted") {
				fmt.Printf("%s[entry %s deleted]\n", pad, gs(e, "id"))
			} else {
				flag := ""
				if unread[gs(e, "id")] {
					flag = " (unread)"
				}
				fmt.Printf("%s[entry %s] %s, %s%s:\n", pad, gs(e, "id"), firstNonEmpty(names[gs(e, "user_id")], "user "+gs(e, "user_id")), c.fmtDate(gs(e, "created_at")), flag)
				text, links := htmlToText(gs(e, "message"), c.base)
				fmt.Println(indent(text, pad+"  "))
				printFileLinks(links)
				atts := gl(e, "attachments")
				if att := walk(e, "attachment"); att != nil && len(atts) == 0 {
					atts = []any{att}
				}
				printAttachments(pad+"  Attached", atts)
			}
			show(gl(e, "replies"), depth+1)
		}
	}
	show(entries, 0)
	return nil
}

func cmdPost(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "reply-to text text-file html file", "")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "post <course | group:ID> <topic-id> (--text TEXT | --text-file PATH | --html HTML) [--reply-to ENTRY_ID] [--file PATH]"); err != nil {
		return err
	}
	ctx, err := c.resolveContext(o.pos[0])
	if err != nil {
		return err
	}
	tid, err := needID("topic", o.pos[1])
	if err != nil {
		return err
	}
	body, err := bodyFromOpts(o, true)
	if err != nil {
		return err
	}
	ctx, tid, _, err = c.topicContext(ctx, tid)
	if err != nil {
		return err
	}
	path := ctx + "/discussion_topics/" + tid + "/entries"
	if r := o.str("reply-to"); r != "" {
		if !isID(r) {
			return errors.New("--reply-to needs an entry ID")
		}
		path += "/" + r + "/replies"
	}
	var res any
	if files := o.vals["file"]; len(files) > 0 {
		if len(files) > 1 {
			return errors.New("a discussion post can have only one attached file")
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("message", body)
		f, err := os.Open(files[0])
		if err != nil {
			return err
		}
		part, _ := mw.CreateFormFile("attachment", filepath.Base(files[0]))
		_, err = io.Copy(part, f)
		f.Close()
		if err != nil {
			return err
		}
		mw.Close()
		res, err = c.call(http.MethodPost, path, nil, mw.FormDataContentType(), &buf)
		if err != nil {
			return err
		}
	} else {
		res, err = c.send(http.MethodPost, path, url.Values{"message": {body}})
		if err != nil {
			return err
		}
	}
	if o.bools["json"] {
		return printJSON(res)
	}
	fmt.Printf("Posted (entry %s) in topic %s.\n", gs(res, "id"), tid)
	return nil
}

// ---- grades, people, groups ----

func cmdGrades(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "all")
	if err != nil {
		return err
	}
	if len(o.pos) == 0 {
		courses, err := c.courseList()
		if err != nil {
			return err
		}
		if o.bools["json"] {
			return printJSON(courses)
		}
		tw := newTable()
		fmt.Fprintln(tw, "ID\tCOURSE\tCURRENT GRADE\tFINAL (ungraded as 0)")
		for _, co := range courses {
			if !o.bools["all"] && !courseCurrent(co) {
				continue
			}
			e := studentEnrollment(co)
			final := "-"
			if gs(e, "computed_final_score") != "" {
				final = pts(e, "computed_final_score") + "% " + gs(e, "computed_final_grade")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", gs(co, "id"), gs(co, "name"), enrollmentGrade(e), final)
		}
		tw.Flush()
		fmt.Println("\nPer-assignment scores: `delta canvas grades <course>`.")
		return nil
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	co, err := c.getObj("courses/"+course, url.Values{"include[]": {"total_scores"}})
	if err != nil {
		return err
	}
	groups, err := c.getAll("courses/"+course+"/assignment_groups", url.Values{"include[]": {"assignments", "submission"}})
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(map[string]any{"course": co, "assignment_groups": groups})
	}
	weighted := gb(co, "apply_assignment_group_weights")
	fmt.Printf("Grades for %s: %s\n", gs(co, "name"), enrollmentGrade(studentEnrollment(co)))
	if weighted {
		fmt.Println("Categories are weighted.")
	}
	for _, g := range groups {
		head := gs(g, "name")
		if weighted {
			head += " (" + pts(g, "group_weight") + "% of grade)"
		}
		if r := walk(g, "rules"); r != nil {
			if n := gs(r, "drop_lowest"); n != "" && n != "0" {
				head += ", drops lowest " + n
			}
			if n := gs(r, "drop_highest"); n != "" && n != "0" {
				head += ", drops highest " + n
			}
		}
		fmt.Printf("\n%s:\n", head)
		tw := newTable()
		for _, a := range gl(g, "assignments") {
			pp := pts(a, "points_possible")
			status := subStatus(walk(a, "submission"), pp)
			if status == "not submitted" {
				if d, ok := c.parseTime(gs(a, "due_at")); ok && d.After(time.Now()) {
					status = "due " + c.fmtTime(gs(a, "due_at"))
				}
			}
			if gb(a, "omit_from_final_grade") {
				status += " (doesn't count)"
			}
			fmt.Fprintf(tw, "  %s\t%s pts\t%s\t[assignment %s]\n", strings.TrimSpace(gs(a, "name")), pp, status, gs(a, "id"))
		}
		tw.Flush()
	}
	return nil
}

func cmdPeople(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "search", "all")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "people <course> [--search NAME] [--all]"); err != nil {
		return err
	}
	course, err := c.resolveCourse(o.pos[0])
	if err != nil {
		return err
	}
	q := url.Values{"include[]": {"enrollments", "email"}}
	if !o.bools["all"] {
		q["enrollment_type[]"] = []string{"teacher", "ta"}
	}
	if s := o.str("search"); s != "" {
		q.Set("search_term", s)
	}
	list, err := c.getAll("courses/"+course+"/users", q)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	tw := newTable()
	fmt.Fprintln(tw, "USER ID\tROLE\tNAME\tEMAIL")
	for _, u := range list {
		var roles []string
		for _, e := range gl(u, "enrollments") {
			if gs(e, "course_id") == course || gs(e, "course_id") == "" {
				role := strings.TrimSuffix(gs(e, "role"), "Enrollment")
				if role == "Ta" {
					role = "TA"
				}
				roles = append(roles, role)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", gs(u, "id"), strings.Join(roles, ","), gs(u, "name"), gs(u, "email"))
	}
	tw.Flush()
	if !o.bools["all"] {
		fmt.Println("\nShowing teachers and TAs; --all lists everyone. Message someone with `delta canvas message --to <user id> ...`.")
	}
	return nil
}

func cmdGroups(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	list, err := c.getAll("users/self/groups", nil)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("Not in any groups.")
		return nil
	}
	tw := newTable()
	fmt.Fprintln(tw, "GROUP\tCOURSE\tMEMBERS\tNAME")
	for _, g := range list {
		fmt.Fprintf(tw, "group:%s\t%s\t%s\t%s\n", gs(g, "id"), gs(g, "course_id"), gs(g, "members_count"), gs(g, "name"))
	}
	tw.Flush()
	fmt.Println("\nUse group:<id> wherever a command takes a course for discussions, pages, files and announcements.")
	return nil
}

// ---- inbox ----

func cmdInbox(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "scope n", "")
	if err != nil {
		return err
	}
	n, err := o.int("n", 20)
	if err != nil {
		return err
	}
	q := url.Values{"per_page": {strconv.Itoa(n)}}
	if s := o.str("scope"); s != "" {
		q.Set("scope", s)
	}
	v, err := c.get("conversations", q)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(v)
	}
	list, _ := v.([]any)
	if len(list) == 0 {
		fmt.Println("No conversations.")
		return nil
	}
	for _, cv := range list {
		var who []string
		for _, p := range gl(cv, "participants") {
			who = append(who, gs(p, "name"))
		}
		flag := ""
		if gs(cv, "workflow_state") == "unread" {
			flag = " (UNREAD)"
		}
		fmt.Printf("- [conversation %s] %s%s\n    %s | %s | %s message(s)\n    %s\n", gs(cv, "id"), firstNonEmpty(gs(cv, "subject"), "(no subject)"), flag,
			c.fmtDate(gs(cv, "last_message_at")), strings.Join(who, ", "), gs(cv, "message_count"), truncate(strings.ReplaceAll(gs(cv, "last_message"), "\n", " "), 200))
	}
	fmt.Println("\nRead one with `delta canvas thread <id>`. Scopes: --scope unread|starred|sent|archived.")
	return nil
}

func cmdThread(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "thread <conversation-id>"); err != nil {
		return err
	}
	id, err := needID("conversation", o.pos[0])
	if err != nil {
		return err
	}
	// Reading doesn't mark the conversation read; the agent may just be searching.
	cv, err := c.getObj("conversations/"+id, url.Values{"auto_mark_as_read": {"false"}})
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(cv)
	}
	names := map[string]string{}
	var who []string
	for _, p := range gl(cv, "participants") {
		names[gs(p, "id")] = gs(p, "name")
		who = append(who, fmt.Sprintf("%s (user %s)", gs(p, "name"), gs(p, "id")))
	}
	fmt.Printf("%s (conversation %s)\nParticipants: %s\n", firstNonEmpty(gs(cv, "subject"), "(no subject)"), id, strings.Join(who, ", "))
	if ctx := gs(cv, "context_name"); ctx != "" {
		fmt.Printf("Course: %s\n", ctx)
	}
	msgs := gl(cv, "messages")
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		fmt.Printf("\n%s, %s:\n%s\n", firstNonEmpty(names[gs(m, "author_id")], "user "+gs(m, "author_id")), c.fmtDate(gs(m, "created_at")), indent(gs(m, "body"), "  "))
		printAttachments("  Attached", gl(m, "attachments"))
	}
	fmt.Printf("\nReply with `delta canvas reply-message %s --text ...`.\n", id)
	return nil
}

func (c *canvasClient) messageAttachments(files []string) ([]string, error) {
	var ids []string
	for _, f := range files {
		file, err := c.upload("users/self/files", f, url.Values{"parent_folder_path": {"conversation attachments"}, "on_duplicate": {"rename"}})
		if err != nil {
			return nil, err
		}
		ids = append(ids, gs(file, "id"))
	}
	return ids, nil
}

func cmdMessage(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "to subject text text-file course file", "group")
	if err != nil {
		return err
	}
	if err := o.need(0, 0, "message --to USER_ID[,USER_ID...] --subject TEXT --text TEXT [--course COURSE] [--file PATH]... [--group]"); err != nil {
		return err
	}
	var to []string
	for _, v := range o.vals["to"] {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				to = append(to, id)
			}
		}
	}
	body, err := plainFromOpts(o)
	if err != nil {
		return err
	}
	if len(to) == 0 || body == "" || o.str("subject") == "" {
		return errors.New("message needs --to, --subject and --text (find user IDs with `delta canvas people <course>`)")
	}
	form := url.Values{"recipients[]": to, "subject": {o.str("subject")}, "body": {body}}
	if arg := o.str("course"); arg != "" {
		id, err := c.resolveCourse(arg)
		if err != nil {
			return err
		}
		form.Set("context_code", "course_"+id)
	}
	if o.bools["group"] {
		form.Set("group_conversation", "true")
	}
	ids, err := c.messageAttachments(o.vals["file"])
	if err != nil {
		return err
	}
	form["attachment_ids[]"] = ids
	res, err := c.send(http.MethodPost, "conversations", form)
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(res)
	}
	list, _ := res.([]any)
	for _, cv := range list {
		fmt.Printf("Sent (conversation %s).\n", gs(cv, "id"))
	}
	if len(list) == 0 {
		fmt.Println("Sent.")
	}
	return nil
}

func cmdReplyMessage(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "text text-file file", "")
	if err != nil {
		return err
	}
	if err := o.need(1, 1, "reply-message <conversation-id> --text TEXT [--file PATH]..."); err != nil {
		return err
	}
	id, err := needID("conversation", o.pos[0])
	if err != nil {
		return err
	}
	body, err := plainFromOpts(o)
	if err != nil {
		return err
	}
	if body == "" {
		return errors.New("give --text or --text-file")
	}
	ids, err := c.messageAttachments(o.vals["file"])
	if err != nil {
		return err
	}
	res, err := c.send(http.MethodPost, "conversations/"+id+"/add_message", url.Values{"body": {body}, "attachment_ids[]": ids})
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(res)
	}
	fmt.Printf("Replied in conversation %s.\n", id)
	return nil
}

// ---- links and raw access ----

type openRoute struct {
	re  *regexp.Regexp
	run func(c *canvasClient, ctx string, m []string, extra []string) error
}

// openRoutes is set in init because some routes call cmdOpen.
var openRoutes []openRoute

// verifier is the file-access token from the URL being opened, if any.
var verifier string

func init() {
	openRoutes = []openRoute{
		{regexp.MustCompile(`^/(courses|groups)/(\d+)/assignments/(\d+)/submissions/(\d+)`), func(c *canvasClient, ctx string, m, x []string) error {
			return cmdSubmission(c, append([]string{m[2], m[3], "--user", m[4]}, x...))
		}},
		{regexp.MustCompile(`^/courses/(\d+)/assignments/(\d+)`), func(c *canvasClient, ctx string, m, x []string) error {
			return cmdAssignment(c, append([]string{m[1], m[2]}, x...))
		}},
		{regexp.MustCompile(`^/(courses|groups)/(\d+)/(?:discussion_topics|announcements)/(\d+)`), func(c *canvasClient, ctx string, m, x []string) error {
			return cmdDiscussion(c, append([]string{ctx, m[3]}, x...))
		}},
		{regexp.MustCompile(`^/(courses|groups)/(\d+)/(?:pages|wiki)/([^/?#]+)`), func(c *canvasClient, ctx string, m, x []string) error {
			return cmdPage(c, append([]string{ctx, m[3]}, x...))
		}},
		{regexp.MustCompile(`^/courses/(\d+)/quizzes/(\d+)`), func(c *canvasClient, ctx string, m, x []string) error {
			return cmdQuiz(c, append([]string{m[1], m[2]}, x...))
		}},
		{regexp.MustCompile(`^/courses/(\d+)/modules/items/(\d+)`), func(c *canvasClient, ctx string, m, x []string) error {
			return c.openModuleItem(m[1], m[2], x)
		}},
		{regexp.MustCompile(`^/courses/(\d+)/modules`), func(c *canvasClient, ctx string, m, x []string) error {
			return cmdModules(c, append([]string{m[1]}, x...))
		}},
		{regexp.MustCompile(`/files/(\d+)`), func(c *canvasClient, ctx string, m, x []string) error {
			f, err := c.getObj("files/"+m[1], url.Values{"verifier": {verifier}})
			if err != nil {
				return err
			}
			size, _ := gf(f, "size")
			ref := m[1]
			if verifier != "" {
				ref = "'" + c.base + "/files/" + m[1] + "?verifier=" + verifier + "'"
			}
			fmt.Printf("File %s: %s (%s, %s). Download with `delta canvas download %s`.\n", m[1], gs(f, "display_name"), humanSize(int64(size)), gs(f, "content-type"), ref)
			return nil
		}},
		{regexp.MustCompile(`^/conversations`), func(c *canvasClient, ctx string, m, x []string) error {
			return cmdInbox(c, x)
		}},
		{regexp.MustCompile(`^/(courses|groups)/(\d+)/?$`), func(c *canvasClient, ctx string, m, x []string) error {
			if m[1] == "groups" {
				return cmdDiscussions(c, append([]string{ctx}, x...))
			}
			return cmdCourse(c, append([]string{m[2]}, x...))
		}},
	}
}

func cmdOpen(c *canvasClient, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: delta canvas open <canvas-url> [--json]")
	}
	u, err := url.Parse(args[0])
	if err != nil || !c.isCanvasURL(args[0]) {
		return fmt.Errorf("%q is not a link to %s", args[0], c.base)
	}
	for _, r := range openRoutes {
		verifier = u.Query().Get("verifier")
		if m := r.re.FindStringSubmatch(u.Path); m != nil {
			ctx := ""
			if mm := regexp.MustCompile(`^/(courses|groups)/(\d+)`).FindStringSubmatch(u.Path); mm != nil {
				ctx = mm[2]
				if mm[1] == "groups" {
					ctx = "group:" + mm[2]
				}
			}
			return r.run(c, ctx, m, args[1:])
		}
	}
	return fmt.Errorf("don't know how to open %s; try `delta canvas api GET %s`", u.Path, u.Path)
}

func (c *canvasClient) openModuleItem(course, item string, extra []string) error {
	seq, err := c.getObj("courses/"+course+"/module_item_sequence", url.Values{"asset_type": {"ModuleItem"}, "asset_id": {item}})
	if err != nil {
		return err
	}
	var cur any
	if items := gl(seq, "items"); len(items) > 0 {
		cur = walk(items[0], "current")
	}
	if cur == nil {
		return fmt.Errorf("module item %s not found", item)
	}
	id := gs(cur, "content_id")
	switch gs(cur, "type") {
	case "Assignment":
		return cmdAssignment(c, append([]string{course, id}, extra...))
	case "Quiz":
		return cmdQuiz(c, append([]string{course, id}, extra...))
	case "Discussion":
		return cmdDiscussion(c, append([]string{course, id}, extra...))
	case "Page":
		return cmdPage(c, append([]string{course, gs(cur, "page_url")}, extra...))
	case "File":
		return cmdOpen(c, append([]string{c.base + "/files/" + id}, extra...))
	default:
		fmt.Printf("%s: %s\n%s\n", gs(cur, "type"), gs(cur, "title"), firstNonEmpty(gs(cur, "external_url"), gs(cur, "html_url")))
		return nil
	}
}

func cmdAPI(c *canvasClient, args []string) error {
	o, err := parseOpts(args, "f body out", "all")
	if err != nil {
		return err
	}
	if err := o.need(2, 2, "api <GET|POST|PUT|DELETE> <path> [-f key=value]... [--body JSON] [--all] [--out FILE]"); err != nil {
		return err
	}
	method, path := strings.ToUpper(o.pos[0]), o.pos[1]
	if o.bools["all"] {
		list, err := c.getAll(path, nil)
		if err != nil {
			return err
		}
		return printJSON(list)
	}
	var body io.Reader
	contentType := ""
	if js := o.str("body"); js != "" {
		body, contentType = strings.NewReader(js), "application/json"
	} else if len(o.vals["f"]) > 0 {
		form := url.Values{}
		for _, kv := range o.vals["f"] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("-f expects key=value, got %q", kv)
			}
			form.Add(k, v)
		}
		if method == http.MethodGet {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			path += sep + form.Encode()
		} else {
			body, contentType = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
		}
	}
	resp, err := c.request(method, path, nil, contentType, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out := o.str("out"); out != "" {
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, resp.Body)
		f.Close()
		if err == nil {
			fmt.Printf("Saved %s (%s).\n", out, humanSize(n))
		}
		return err
	}
	v, err := decodeJSON(resp.Body)
	if err != nil {
		return err
	}
	return printJSON(v)
}
