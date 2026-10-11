package main

const canvasHelp = `delta canvas: full student access to the user's Canvas (ASU)

HOW IT WORKS
  The command center holds the user's logged-in Canvas session and relays
  your requests to Canvas with it. You never see a password or cookie. Run
  "delta set-auth" first (see "delta help"). "delta canvas status" checks
  the connection. If it says the session is not ready, Canvas needs a fresh
  login. You can't do that, so tell the user.

  Everything you do here is done AS THE STUDENT and is visible to their
  instructors and classmates. Before anything that can't be taken back
  (submitting, posting, messaging, starting or turning in a quiz), make sure
  the user asked for it. Use "submit --dry-run" to check a submission first.

CONVENTIONS
  <course>  A course ID (273293), a Canvas course URL, or part of the course
            code or name ("thf101", "storytelling"). Must match one course.
  <ctx>     A <course>, or group:<id> for a student group (see "groups").
            Accepted by discussions, discussion, post, pages, page, files.
  IDs       Every list prints the IDs the next command needs. Canvas URLs
            are accepted in place of IDs, and "open <url>" shows any
            Canvas link.
  Text      Canvas content is shown as Markdown. Write with --text "..." or
            --text-file FILE in Markdown (headings, **bold**, *italic*,
            lists, links, > quotes, tables). Newlines inside a paragraph
            are kept, blank lines start new paragraphs, and inline HTML
            passes through (e.g. <p style="text-align:center">). A .html
            --text-file, or --html "...", is sent as raw HTML. Inbox
            messages and submission comments are plain text.
  Times     Shown in the user's Canvas time zone, with "in 2d 3h"/"5h ago".
  --json    Most commands accept --json for the raw Canvas data.

FINDING WORK
  status                           Connection check and who is logged in.
  courses [--all]                  Current courses with grades (--all: past too).
  course <course>                  Details, teachers, sections, syllabus.
  todo [--days 7] [--past 7] [--course C]... [--all]
                                   Everything due or scheduled across courses
                                   (assignments, quizzes, discussions, peer
                                   reviews, events). Finished work is hidden.
  missing                          Past-due work that was never submitted.
  recent [--course C] [--n 25]     New grades, comments, announcements, messages.
  announcements [course...] [--days 14] [--full]
  modules <course>                 The course's content in order. Most
                                   courses organize everything here.
  mark-done <course> <module> <item> [--undo]
  mark-viewed <course> <module> <item>
                                   Satisfy "mark as done" / "view" requirements.
  pages <ctx> [--search T] / page <ctx> <page-url|front>
  files <ctx|me> [--search T]      Many courses hide Files; use modules.
  download <file-id|url> [--out PATH] [--to-phone]
                                   Save a Canvas file (or any linked URL)
                                   locally. --to-phone also pushes it to
                                   the phone's files list.
  upload <path>... [--to me|group:ID] [--folder PATH]
                                   Put files in the user's (or a group's)
                                   Canvas files and print their IDs.
  open <canvas-url>                Show whatever a Canvas link points to.

ASSIGNMENTS
  assignments <course> [--bucket upcoming|overdue|past|undated|unsubmitted]
              [--search T]
  assignment <course> <id>         Instructions, due/lock dates, allowed
                                   submission types and file extensions,
                                   rubric, linked files, your status.
  submit <course> <id> --file PATH [--file PATH]...   (online_upload)
  submit <course> <id> --text-file essay.md           (online_text_entry)
  submit <course> <id> --text "..." | --html "..."    (online_text_entry)
  submit <course> <id> --url https://...              (online_url)
      [--comment "note to instructor"] [--dry-run]
                                   Checks the assignment accepts that type
                                   and file extension, isn't locked, and has
                                   attempts left. It then uploads files and
                                   submits. A resubmission is a new attempt.
  submission <course> <id> [--user U]
                                   What was submitted, grade, rubric scores,
                                   instructor comments and their files.
  comment <course> <id> --text "..." [--file PATH]... [--user U]
                                   Comment on your submission (or, with
                                   --user, on a peer's when peer reviewing).
  peer-reviews <course> <id>       Peer reviews you must do / received.

  Assignments of type discussion_topic are submitted by posting in the
  discussion. online_quiz means a Classic Quiz (see QUIZZES). "new quiz",
  external_tool, media_recording and student_annotation need a browser.

DISCUSSIONS AND ANNOUNCEMENTS
  discussions <ctx>                Topics with reply counts and due dates.
  discussion <ctx> <topic>         Prompt plus all replies as a thread, with
                                   entry IDs. Announcements are topics too.
  post <ctx> <topic> --text "..." [--reply-to ENTRY] [--file PATH]
                                   New post, or a reply to an entry. In
                                   group discussions your group's copy is
                                   used automatically. Some topics hide
                                   replies until you post.

QUIZZES (Classic Quizzes only; New Quizzes have no API)
  quizzes <course> / quiz <course> <quiz>     Info, time limit, attempts.
  quiz-start <course> <quiz> [--access-code C]
                                   Starts (or resumes) an attempt. This uses
                                   an attempt and starts the timer. It then
                                   prints the questions with answer IDs.
  quiz-questions <course> <quiz>   Re-show questions and saved answers.
  quiz-answer <course> <quiz> <question-id> <answer>
  quiz-answer <course> <quiz> --answers-file answers.json   ({"qid": answer})
                                   Saves answers (changeable until turned in).
                                   Multiple choice/true-false: answer ID.
                                   Multiple answers: IDs "12,15". Short
                                   answer/numeric/essay: text. Fill in blanks:
                                   '{"blank":"text"}'. Dropdowns:
                                   '{"blank":ID}'. Matching:
                                   '[{"answer_id":1,"match_id":2}]'.
  quiz-submit <course> <quiz>      Turn the attempt in.

GRADES, PEOPLE, MESSAGES
  grades [course] [--all]          All course grades, or one course's
                                   assignments by category, weights, drops.
  people <course> [--search NAME] [--all]   Teachers/TAs (--all: everyone).
  groups                           Your student groups (group:<id>).
  inbox [--scope unread|starred|sent|archived] [--n 20]
  thread <conversation-id>         Read a conversation.
  message --to USER_ID[,USER_ID] --subject S --text "..." [--course C]
          [--file PATH]... [--group]
                                   Send a Canvas inbox message (e.g. to an
                                   instructor; find IDs with "people").
  reply-message <conversation-id> --text "..." [--file PATH]...

ANYTHING ELSE
  api <METHOD> <path> [-f key=value]... [--body JSON] [--all] [--out FILE]
      Raw Canvas REST API as the student, e.g.
        delta canvas api GET courses/273293/assignments/5/submissions/self
        delta canvas api PUT planner/overrides/12 -f marked_complete=true
      Paths may omit /api/v1/. --all follows pagination. Reference:
      https://canvas.instructure.com/doc/api/

EXAMPLE: the user sends a photo of handwritten work for an assignment
  1. delta canvas todo  (or assignments <course>) to find the assignment ID.
  2. delta canvas assignment <course> <id> to read the instructions, the
     accepted submission types and the allowed file extensions.
  3. Transcribe the photo. Produce what the assignment wants: Markdown for
     a text entry, or a file it accepts (convert Markdown to .docx/.pdf
     with tools like pandoc or LibreOffice). You can also upload the photo
     itself if images are accepted.
  4. delta canvas submit <course> <id> --file work.pdf --dry-run, confirm
     with the user, then run it again without --dry-run.
  5. delta canvas submission <course> <id> to verify it arrived.
  Files the user sends from the phone are in its files list; fetch them
  with "delta pull <name>".
`
