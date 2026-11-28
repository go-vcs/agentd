package ghapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// maxService bounds a keychain item name.
const maxService = 128

// keychainTimeout bounds one lookup: a locked keychain may wait for a person.
var keychainTimeout = 30 * time.Second

// keychainCommand names the program and arguments that print the secret of the
// item `service` on standard output, and extra environment for it. Replaced by
// tests. The service name is the only thing on the command line; the secret
// comes back on a pipe, never through argv, the environment or a file.
var keychainCommand = func(service string) (bin string, args, env []string, err error) {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/bin/security", []string{"find-generic-password", "-s", service, "-w"}, nil, nil
	case "linux":
		return "/usr/bin/secret-tool", []string{"lookup", "service", service}, nil, nil
	}
	return "", nil, nil, fmt.Errorf("keychain: no system keychain on %s", runtime.GOOS)
}

// validService accepts letters, digits, dot, underscore and hyphen, not
// starting with a hyphen: a name that cannot be read as an option or a path.
func validService(s string) bool {
	if s == "" || len(s) > maxService || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// capped is a writer that refuses more than max bytes: a keychain program that
// prints without end costs the daemon max bytes, then its process.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool // a write was refused: the process may die of it, so Run's error is not the cause
}

var errTooMuch = errors.New("output over the bound")

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.over = true
		return 0, errTooMuch
	}
	return c.buf.Write(p)
}

// ReadKeychainToken returns the secret of the system keychain item named
// service (macOS: a generic password; Linux: a Secret Service item with the
// attribute service=<name>, through secret-tool). The program is run by absolute
// path with a minimal environment, bounded in time and output; its standard
// error is dropped, and no error text carries what it printed.
func ReadKeychainToken(ctx context.Context, service string) (string, error) {
	if !validService(service) {
		return "", errors.New("keychain: the item name must be 1 to 128 letters, digits, dots, underscores or hyphens, not starting with one")
	}
	bin, args, extra, err := keychainCommand(service)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(keychainBaseEnv(), extra...)
	out := &capped{max: maxToken + 1}
	cmd.Stdout = out
	err = cmd.Run()
	switch {
	case out.over:
		return "", errors.New("keychain: larger than 1 KiB")
	case ctx.Err() != nil:
		return "", errors.New("keychain: no answer in time")
	case err != nil:
		return "", fmt.Errorf("keychain: lookup of %q failed", service)
	}
	return parseToken(out.buf.Bytes(), "keychain")
}

// keychainBaseEnv is what a keychain program needs to find the user's keychain
// and nothing else.
func keychainBaseEnv() []string {
	var env []string
	for _, k := range []string{"HOME", "USER", "LOGNAME", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok && !strings.ContainsRune(v, 0) {
			env = append(env, k+"="+v)
		}
	}
	return env
}
