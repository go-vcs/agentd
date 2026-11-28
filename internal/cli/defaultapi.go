package cli

import (
	"net/url"
	"strings"
)

// isDefaultAPI reports whether s names GitHub's own API however it is spelled
// (trailing slash, host in capitals), so that its git host is the default too.
func isDefaultAPI(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.github.com") &&
		u.Port() == "" && strings.Trim(u.Path, "/") == ""
}
