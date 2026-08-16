// Internal tests for the bash-tool platform helpers: shell selection and
// sanitized PATH construction must match the platform (cmd.exe /C +
// System32 on Windows, /bin/sh -c + POSIX dirs elsewhere).
package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBashShellSelection(t *testing.T) {
	shell, arg := bashShell()
	if runtime.GOOS == "windows" {
		if shell != "cmd.exe" || arg != "/C" {
			t.Fatalf("windows shell: got (%q, %q), want (cmd.exe, /C)", shell, arg)
		}
	} else {
		if shell != "/bin/sh" || arg != "-c" {
			t.Fatalf("unix shell: got (%q, %q), want (/bin/sh, -c)", shell, arg)
		}
	}
}

func TestBashPathEnv(t *testing.T) {
	dir := t.TempDir()
	// A project venv must be picked up on both layouts (bin on unix,
	// Scripts on Windows) whichever one exists on this platform.
	_ = os.MkdirAll(filepath.Join(dir, ".venv", "bin"), 0o755)
	env := bashPathEnv(dir)
	if !strings.Contains(env, filepath.Join(dir, ".venv", "bin")) {
		t.Fatalf("venv bin missing from PATH: %q", env)
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(env, "System32") {
			t.Fatalf("System32 missing from PATH: %q", env)
		}
	} else {
		for _, want := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
			if !strings.Contains(env, want) {
				t.Fatalf("POSIX dir %s missing from PATH: %q", want, env)
			}
		}
	}
}
