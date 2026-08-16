// Ported from tests/test_config.py (102 lines): key-store, resolution
// order, cost estimation, and catalog invariants (offline, deterministic).
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/config"
)

func setupXDG(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, "config")
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	return cfgDir
}

func TestSaveLoadRoundTrip(t *testing.T) {
	cfgDir := setupXDG(t)
	path := filepath.Join(cfgDir, "kaal", "api_key")
	if key := config.LoadUserAPIKey(); key != "" {
		t.Fatalf("fresh store must be empty, got %q", key)
	}
	if err := config.SaveUserAPIKey("sk-roundtrip"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "sk-roundtrip" { // no newline
		t.Fatalf("file content: %q", raw)
	}
	if got := config.LoadUserAPIKey(); got != "sk-roundtrip" {
		t.Fatalf("load: %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions: %o", perm)
	}
}

func TestEnvWinsOverUserStore(t *testing.T) {
	setupXDG(t)
	if err := config.SaveUserAPIKey("sk-user"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_API_KEY", "sk-env")
	got, err := config.GetAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-env" {
		t.Fatalf("got %q", got)
	}
}

func TestUserStoreWinsOverOmpDB(t *testing.T) {
	setupXDG(t)
	if err := config.SaveUserAPIKey("sk-user"); err != nil {
		t.Fatal(err)
	}
	// With no real ~/.omp store, the user store must resolve.
	t.Setenv("HOME", t.TempDir())
	got, err := config.GetAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-user" {
		t.Fatalf("got %q", got)
	}
}

func TestNoKeyError(t *testing.T) {
	setupXDG(t)
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("HOME", t.TempDir())
	_, err := config.GetAPIKey()
	if err == nil {
		t.Fatal("want error without any key")
	}
	if !strings.Contains(err.Error(), "OPENCODE_API_KEY") {
		t.Fatalf("error must mention the env var: %v", err)
	}
}

func TestEstimateCostZeroTokens(t *testing.T) {
	if got := config.EstimateCost(0, 0, 0, ""); got != 0.0 {
		t.Fatalf("got %v", got)
	}
}

func TestEstimateCost1MEach(t *testing.T) {
	// input $0.14/M + output $0.28/M = $0.42 per 1M tokens each.
	if got := config.EstimateCost(1_000_000, 1_000_000, 0, ""); got != 0.42 {
		t.Fatalf("got %v", got)
	}
}

func TestEstimateCostCacheReadTerms(t *testing.T) {
	if got := config.EstimateCost(0, 0, 1_000_000, ""); got != 0.0028 {
		t.Fatalf("got %v", got)
	}
}

func TestModelStoreRoundTrip(t *testing.T) {
	setupXDG(t)
	if got := config.LoadUserModel(); got != "" {
		t.Fatalf("fresh model store must be empty, got %q", got)
	}
	if err := config.SaveUserModel("deepseek-v4-pro"); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadUserModel(); got != "deepseek-v4-pro" {
		t.Fatalf("load: %q", got)
	}
	// Resolution order: flag > saved > ModelID.
	if got := config.ResolveModelID(""); got != "deepseek-v4-pro" {
		t.Fatalf("saved model: %q", got)
	}
	if got := config.ResolveModelID("kimi-k2.5"); got != "kimi-k2.5" {
		t.Fatalf("flag model: %q", got)
	}
}

func TestCatalogSortedFreeFirstAscending(t *testing.T) {
	if config.Models[0].ID != "deepseek-v4-flash-free" {
		t.Fatalf("first model: %s", config.Models[0].ID)
	}
	prev := -1.0
	for _, m := range config.Models {
		if m.InputPerM < prev {
			t.Fatalf("catalog not ascending at %s (%v < %v)", m.ID, m.InputPerM, prev)
		}
		prev = m.InputPerM
	}
	free := []config.Model{}
	for _, m := range config.Models {
		if m.BaseURL != "" {
			free = append(free, m)
		}
	}
	if len(free) == 0 {
		t.Fatal("no free-tier models")
	}
	for _, m := range free {
		if m.InputPerM != 0.0 || m.BaseURL != config.FreeBaseURL {
			t.Fatalf("free model %s: %+v", m.ID, m)
		}
	}
}

func TestModelRatesAndBaseURLs(t *testing.T) {
	if in, out := config.ModelRates("deepseek-v4-flash"); in != 0.14 || out != 0.28 {
		t.Fatalf("flash rates: %v %v", in, out)
	}
	if in, out := config.ModelRates("deepseek-v4-flash-free"); in != 0.0 || out != 0.0 {
		t.Fatalf("free rates: %v %v", in, out)
	}
	if in, out := config.ModelRates("unknown-model"); in != 0.14 || out != 0.28 {
		t.Fatalf("fallback rates: %v %v", in, out)
	}
	if got := config.ModelBaseURL("deepseek-v4-flash"); got != config.BaseURL {
		t.Fatalf("paid base: %s", got)
	}
	if got := config.ModelBaseURL("deepseek-v4-flash-free"); got != config.FreeBaseURL {
		t.Fatalf("free base: %s", got)
	}
}

func TestEstimateCostUsesModelRates(t *testing.T) {
	if got := config.EstimateCost(1_000_000, 1_000_000, 0, "deepseek-v4-flash-free"); got != 0.0 {
		t.Fatalf("free cost: %v", got)
	}
	if got := config.EstimateCost(1_000_000, 1_000_000, 0, "deepseek-v4-pro"); got != 1.305 {
		t.Fatalf("pro cost: %v", got)
	}
}
