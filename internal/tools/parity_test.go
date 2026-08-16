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

func TestResolveRelativeSymlinkedBase(t *testing.T) {
	// Regression: a cwd whose prefix is a symlink (macOS /var -> /private/var)
	// must not make in-project paths look like escapes — the base must be
	// resolved the same way as the target.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	target := filepath.Join(link, "a.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRelative("a.txt", link)
	if err != nil {
		t.Fatalf("in-project path rejected through symlinked base: %v", err)
	}
	if got != filepath.Join(real, "a.txt") {
		t.Fatalf("resolved %q, want %q", got, filepath.Join(real, "a.txt"))
	}
	// Escapes are still rejected.
	if _, err := ResolveRelative("../outside", link); err == nil {
		t.Fatal("escape must still be blocked")
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
