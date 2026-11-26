package status

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want string
	}{
		{"nil", nil, ""},
		{"plain text", errors.New("unexpected status code: 502"), "unexpected status code: 502"},
		{
			"query fragment and userinfo",
			errors.New(`Get "https://user:pw@api.example.com/v1/x?token=abc&b=2#frag": dial tcp: refused`),
			`Get "https://api.example.com/v1/x": dial tcp: refused`,
		},
		{
			"several urls",
			errors.New("a http://h1/p?k=1 then https://h2:8443/q?secret=2"),
			"a http://h1/p then https://h2:8443/q",
		},
		{"control characters", errors.New("line1\nline2\tx\r"), "line1 line2 x "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.in); got != tt.want {
				t.Errorf("Sanitize = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeTruncatesOnRuneBoundary(t *testing.T) {
	got := Sanitize(errors.New(strings.Repeat("é", 400))) // 800 bytes
	if len(got) > MaxErrorBytes {
		t.Fatalf("len = %d", len(got))
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %q", got)
	}
	short := strings.Repeat("a", MaxErrorBytes)
	if Sanitize(errors.New(short)) != short {
		t.Fatal("text at the limit must be unchanged")
	}
}
