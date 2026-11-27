package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMaterializeSendsDirAndRepeatedPaths(t *testing.T) {
	sock := fakeDaemon(t, "ok")
	out, _, err := runAgent(t, "--label", "x", "--socket", sock, "materialize", "o/r", id40, "w/1", "--path", "a", "--path", "b/c")
	if err != nil || !strings.Contains(out, `"dir":"/root/w/1"`) || !strings.Contains(out, `"files":2`) {
		t.Fatalf("%q %v", out, err)
	}
}

func TestMaterializeReportsARefusalAndNeedsThreeArguments(t *testing.T) {
	sock := fakeDaemon(t, "refuse")
	out, errb, err := runAgent(t, "--label", "x", "--socket", sock, "materialize", "o/r", id40, "d")
	ee, ok := err.(exitError)
	if !ok || ee.code != ExitError || out != "" || !strings.Contains(errb, `"reason":"OBJECT_MISMATCH"`) {
		t.Fatalf("%v out %q err %q", err, out, errb)
	}
	if _, _, err := runAgent(t, "--label", "x", "--socket", sock, "materialize", "o/r", id40); err == nil {
		t.Fatal("two arguments accepted")
	}
	if _, _, err := runAgent(t, "--socket", sock, "materialize", "o/r", id40, "d"); err == nil || !strings.Contains(err.Error(), "--label") {
		t.Fatalf("no label: %v", err)
	}
	if _, errb, err := runAgent(t, "--label", "x", "--socket", "/nonexistent/s", "materialize", "o/r", id40, "d"); err == nil || !strings.Contains(errb, `"reason":"UNAVAILABLE"`) {
		t.Fatalf("unreachable: %v %q", err, errb)
	}
}

func TestPrepareRootCreatesPrivateDurableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	base, err := os.MkdirTemp(home, ".agentd-test-")
	if err != nil {
		t.Skip("cannot create a directory under the home")
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	got, err := PrepareRoot(filepath.Join(base, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi, err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("%q is not absolute", got)
	}
}

func TestPrepareRootRefusesTheTemporaryDirectory(t *testing.T) {
	for _, d := range []string{os.TempDir(), filepath.Join(os.TempDir(), "agentd-x", "y")} {
		if got, err := PrepareRoot(d); err == nil {
			t.Errorf("%s accepted as %s", d, got)
		}
	}
	if _, err := PrepareRoot(filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("a subdirectory of the temporary directory accepted")
	}
	// A path that cannot be created.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRoot(filepath.Join(f, "sub")); err == nil {
		t.Error("a directory below a file accepted")
	}
}
