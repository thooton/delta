package main

import (
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
