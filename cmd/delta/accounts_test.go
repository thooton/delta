package main

import "testing"

func TestParseLogin(t *testing.T) {
	cases := []struct {
		in   string
		want cloudLogin
	}{
		{`{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":1700000000000,"scopes":["user:inference"]}}`,
			cloudLogin{AccessToken: "a1", RefreshToken: "r1", ExpiresAt: 1700000000000}},
		{`{"anthropic":{"type":"oauth","access":"a2","refresh":"r2","expires":1700000000001}}`,
			cloudLogin{AccessToken: "a2", RefreshToken: "r2", ExpiresAt: 1700000000001}},
		{`{"access_token":"a3","refresh_token":"r3","expires_at":5}`,
			cloudLogin{AccessToken: "a3", RefreshToken: "r3", ExpiresAt: 5}},
		{`{"refreshToken":"r4"}`, cloudLogin{RefreshToken: "r4"}},
		{"sk-ant-ort01-abc\n", cloudLogin{RefreshToken: "sk-ant-ort01-abc"}},
	}
	for _, c := range cases {
		got, err := parseLogin([]byte(c.in))
		if err != nil || got != c.want {
			t.Errorf("parseLogin(%s) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "{}", `{"anthropic":{"access":"a"}}`, "not a token"} {
		if _, err := parseLogin([]byte(bad)); err == nil {
			t.Errorf("parseLogin(%q) succeeded", bad)
		}
	}
}
