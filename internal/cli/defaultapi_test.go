package cli

import "testing"

func TestIsDefaultAPI(t *testing.T) {
	for s, want := range map[string]bool{
		"https://api.github.com":          true,
		"https://api.github.com/":         true,
		"https://API.GitHub.com":          true,
		"http://api.github.com":           false,
		"https://api.github.com:8443":     false,
		"https://api.github.com/v3":       false,
		"https://api.github.com.evil.org": false,
		"https://ghe.example.org/api/v3":  false,
		"http://127.0.0.1:1":              false,
		"":                                false,
	} {
		if got := isDefaultAPI(s); got != want {
			t.Errorf("%q: %v, want %v", s, got, want)
		}
	}
}
