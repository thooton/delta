package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCheckEmail(t *testing.T) {
	// As Python's email package writes one, which transform help tells the agent to use.
	python := "To: Ms. Rivera <rivera@school.org>, sam@x.org\nSubject: Late homework =?utf-8?b?4oCU?= sorry\nX-Unsent: 1\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\nContent-Transfer-Encoding: 8bit\nMIME-Version: 1.0\n\nDear Ms. Rivera,\n\nI’ll hand it in tomorrow.\n\nSam\n"
	for _, c := range []struct{ eml, want string }{
		{python, ""},
		{"Subject: \r\n\r\nNo one to send it to yet.", ""},
		{"not an email", "isn't an email message"},
		{"To: a@b.org\n\nHi", "Subject"},
		{"From: a@b.org\nSubject: Hi\n\nHi", "From"},
		{"To: Ms. Rivera\nSubject: Hi\n\nHi", "addresses"},
		{"Subject: Hi\nContent-Type: text/html\n\n<p>Hi", "plain text"},
	} {
		got := checkEmail([]byte(c.eml))
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("checkEmail(%q) = %q, want %q", c.eml, got, c.want)
		}
	}
}

func TestFormatsWithoutPDF(t *testing.T) {
	if got, _ := checkFormats(nil); !slices.Equal(got, []string{"pdf"}) {
		t.Errorf("no formats: %v, want a PDF", got)
	}
	if got, _ := checkFormats([]string{"eml", "docx"}); !slices.Equal(got, []string{"docx", "eml"}) {
		t.Errorf("formats without a PDF: %v", got)
	}
	email := submission{"eml": {[]byte("To: a@b.org\nSubject: Hi\n\nHi")}}
	if why := checkSubmission(email, []string{"eml"}); why != "" {
		t.Errorf("an email alone: %s", why)
	}
	if why := checkSubmission(email, []string{"pdf", "eml"}); !strings.Contains(why, "PDF") {
		t.Errorf("a PDF asked for but missing: %q", why)
	}
	if why := checkSubmission(email, nil); !strings.Contains(why, "PDF") {
		t.Errorf("an old job always makes a PDF: %q", why)
	}
}

func TestCheckFlashcards(t *testing.T) {
	for _, c := range []struct{ fkv, want string }{
		{"hola||hello\nadiós||goodbye\n", ""},
		{"\n\n", "no cards"},
		{"hola||hello\nadiós goodbye\n", "line 2"},
		{"a||" + strings.Repeat("x", maxFlashcardText+1), "too long"},
	} {
		got := checkFlashcards([]byte(c.fkv))
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("checkFlashcards(%q) = %q, want %q", c.fkv, got, c.want)
		}
	}
	if got, _ := checkFormats([]string{"jpg", "fkv", "pdf"}); !slices.Equal(got, []string{"pdf", "fkv", "jpg"}) {
		t.Errorf("formats with flashcards: %v", got)
	}
	cards := submission{"fkv": {[]byte("hola||hello\n")}}
	if why := checkSubmission(cards, []string{"fkv"}); why != "" {
		t.Errorf("flashcards alone: %s", why)
	}
}
