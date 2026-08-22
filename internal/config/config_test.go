// Ported from tests/test_config.py (102 lines): key-store, resolution
// order, cost estimation, and catalog invariants (offline, deterministic).
package config_test

import (
	"os"
	"path/filepath"
	"runtime"
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
	if perm := info.Mode().Perm(); perm != 0o600 && runtime.GOOS != "windows" {
		// Windows has no unix permission bits; the mode is best-effort there.
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
		if m.InputPerM == 0.0 && m.OutputPerM == 0.0 {
			free = append(free, m)
		}
	}
	if len(free) == 0 {
		t.Fatal("no free-tier models")
	}
	for _, m := range free {
		// Zero-rate entries may ride the free tier, a Command Code deal, or
		// the paid endpoint's own promos (ox-alpha-free on zen/go).
		if m.BaseURL != config.FreeBaseURL &&
			m.BaseURL != config.CommandCodeBaseURL &&
			m.BaseURL != config.BaseURL {
			t.Fatalf("free model %s routes to %q", m.ID, m.BaseURL)
		}
	}
	// Ids are unique across the catalog — lookups must be unambiguous.
	seen := map[string]bool{}
	for _, m := range config.Models {
		if seen[m.ID] {
			t.Fatalf("duplicate model id: %s", m.ID)
		}
		seen[m.ID] = true
	}
}

func TestDefaultModelIsFreeTier(t *testing.T) {
	setupXDG(t)
	if got := config.ResolveModelID(""); got != config.ModelID {
		t.Fatalf("default resolution: %q", got)
	}
	if !config.FreeTierModel(config.ModelID) {
		t.Fatalf("shipped default must ride the keyless free tier: %q", config.ModelID)
	}
	if base := config.ModelBaseURL(config.ModelID); base != config.FreeBaseURL {
		t.Fatalf("default base url: %s", base)
	}
}

func TestFreeTierModelRouting(t *testing.T) {
	cases := map[string]bool{
		"hy3-free":                true,
		"deepseek-v4-flash-free":  true,
		"big-pickle":              true, // zen promo models ride the free tier
		"x-preview-f-free":        true,
		"deepseek-v4-flash":       false,
		"stealth/ox-alpha":        false,
		"google/gemini-3.7-flash": false,
		"totally-unknown-model":   false, // unknown ids are treated as keyed
	}
	for id, want := range cases {
		if got := config.FreeTierModel(id); got != want {
			t.Fatalf("freeTier(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestModelProviderRouting(t *testing.T) {
	cases := map[string]string{
		"deepseek-v4-flash-free":     config.ProviderOpencode,
		"deepseek-v4-flash":          config.ProviderOpencode,
		"stealth/ox-alpha":           config.ProviderCommandCode,
		"ling-3.0-flash":             config.ProviderCommandCode,
		"deepseek/deepseek-v4-flash": config.ProviderCommandCode,
		"google/gemini-3.7-flash":    config.ProviderCommandCode,
		"totally-unknown-model":      config.ProviderOpencode, // unknown ids ride the main gateway
	}
	for id, want := range cases {
		if got := config.ModelProvider(id); got != want {
			t.Fatalf("provider(%q) = %q, want %q", id, got, want)
		}
	}
	if got := config.ModelBaseURL("stealth/ox-alpha"); got != config.CommandCodeBaseURL {
		t.Fatalf("command code base: %s", got)
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

// -- Command Code provider ------------------------------------------------------

func TestCommandCodeKeyResolution(t *testing.T) {
	setupXDG(t)
	// The commandcode chain must not consult opencode's env or store.
	t.Setenv("OPENCODE_API_KEY", "sk-opencode")
	t.Setenv("CMD_API_KEY", "cmd-env")
	got, err := config.GetAPIKeyFor(config.ProviderCommandCode)
	if err != nil || got != "cmd-env" {
		t.Fatalf("CMD_API_KEY must win: %q %v", got, err)
	}
	// COMMANDCODE_API_KEY is the long-form alias.
	t.Setenv("CMD_API_KEY", "")
	t.Setenv("COMMANDCODE_API_KEY", "cmd-long")
	if got, err = config.GetAPIKeyFor(config.ProviderCommandCode); err != nil || got != "cmd-long" {
		t.Fatalf("COMMANDCODE_API_KEY alias: %q %v", got, err)
	}
	// Then the dedicated user store.
	t.Setenv("COMMANDCODE_API_KEY", "")
	if err := config.SaveUserAPIKeyFor(config.ProviderCommandCode, "cmd-store"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(setupXDGPath(t), "kaal", "api_key.commandcode")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "cmd-store" {
		t.Fatalf("store content: %q", raw)
	}
	if got, err = config.GetAPIKeyFor(config.ProviderCommandCode); err != nil || got != "cmd-store" {
		t.Fatalf("user store: %q %v", got, err)
	}
	// And the error names the right env vars.
	setupXDG(t)
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.commandcode auth
	_, err = config.GetAPIKeyFor(config.ProviderCommandCode)
	if err == nil || !strings.Contains(err.Error(), "CMD_API_KEY") {
		t.Fatalf("error must mention CMD_API_KEY: %v", err)
	}
}

func TestPerProviderStoreIsolation(t *testing.T) {
	setupXDG(t)
	if err := config.SaveUserAPIKeyFor(config.ProviderOpencode, "sk-zen"); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveUserAPIKeyFor(config.ProviderCommandCode, "sk-cmd"); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadUserAPIKey(); got != "sk-zen" {
		t.Fatalf("opencode store: %q", got)
	}
	if got := config.LoadUserAPIKeyFor(config.ProviderCommandCode); got != "sk-cmd" {
		t.Fatalf("commandcode store: %q", got)
	}
}

// setupXDGPath returns the XDG config dir from setupXDG for path assertions.
func setupXDGPath(t *testing.T) string {
	t.Helper()
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		t.Fatal("XDG_CONFIG_HOME not set by setupXDG")
	}
	return base
}

func TestCustomProviderRoutingAndKey(t *testing.T) {
	setupXDG(t)
	t.Setenv("CUSTOM_API_KEY", "")
	t.Setenv("TOGETHER_API_KEY", "")
	if err := config.ClearCustomProvider(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.ClearCustomProvider() })

	// The name is sanitized on save; BaseURL keeps its shape and lookups trim.
	if err := config.SaveCustomProvider(config.CustomProvider{
		Name: "Together", BaseURL: "https://api.together.xyz/v1/", APIKey: "sk-tog", Model: "gpt-x",
	}); err != nil {
		t.Fatal(err)
	}
	got := config.LoadCustomProvider()
	if got == nil || got.Name != "together" || got.Model != "gpt-x" {
		t.Fatalf("roundtrip: %+v", got)
	}
	if base := config.ModelBaseURL("gpt-x"); base != "https://api.together.xyz/v1" {
		t.Fatalf("custom routing: %s", base)
	}
	if p := config.ModelProvider("gpt-x"); p != "together" {
		t.Fatalf("provider: %s", p)
	}
	// Built-ins stay untouched while another id is requested.
	if p := config.ModelProvider("deepseek-v4-flash-free"); p != config.ProviderOpencode {
		t.Fatalf("builtin provider: %s", p)
	}

	// Key chain: stored key, then the derived env var wins over it.
	if key, err := config.GetAPIKeyFor("together"); err != nil || key != "sk-tog" {
		t.Fatalf("stored key: %q %v", key, err)
	}
	t.Setenv("TOGETHER_API_KEY", "sk-env")
	if key, err := config.GetAPIKeyFor("together"); err != nil || key != "sk-env" {
		t.Fatalf("env key: %q %v", key, err)
	}

	// Clearing removes the override entirely.
	if err := config.ClearCustomProvider(); err != nil {
		t.Fatal(err)
	}
	if config.LoadCustomProvider() != nil {
		t.Fatal("provider survived ClearCustomProvider")
	}
	if base := config.ModelBaseURL("gpt-x"); strings.Contains(base, "together") {
		t.Fatalf("still routed after clear: %s", base)
	}
}

func TestCommandCodeAuthFileChain(t *testing.T) {
	setupXDG(t)
	t.Setenv("CMD_API_KEY", "")
	t.Setenv("COMMANDCODE_API_KEY", "")
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	writeHome := func(rel, content string) {
		t.Helper()
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// No key anywhere → the error names both env vars and points at the CLI.
	if _, err := config.GetAPIKeyFor(config.ProviderCommandCode); err == nil {
		t.Fatal("want error with no sources")
	} else if !strings.Contains(err.Error(), "CMD_API_KEY") || !strings.Contains(err.Error(), "command-code CLI") {
		t.Fatalf("error text: %v", err)
	}

	// The official CLI's shape.
	writeHome(filepath.Join(".commandcode", "auth.json"), `{"apiKey":"user_cli"}`)
	if got, err := config.GetAPIKeyFor(config.ProviderCommandCode); err != nil || got != "user_cli" {
		t.Fatalf("cli auth file: %q %v", got, err)
	}

	// pi's oauth credential under "commandcode".
	os.Remove(filepath.Join(home, ".commandcode", "auth.json"))
	writeHome(filepath.Join(".pi", "agent", "auth.json"),
		`{"commandcode":{"type":"oauth","access":"tok_pi"}}`)
	if got, _ := config.GetAPIKeyFor(config.ProviderCommandCode); got != "tok_pi" {
		t.Fatalf("pi oauth: %q", got)
	}

	// OMP's api credential under "command-code".
	os.Remove(filepath.Join(home, ".pi", "agent", "auth.json"))
	writeHome(filepath.Join(".omp", "agent", "auth.json"),
		`{"command-code":{"type":"api","key":"user_omp"}}`)
	if got, _ := config.GetAPIKeyFor(config.ProviderCommandCode); got != "user_omp" {
		t.Fatalf("omp credential: %q", got)
	}

	// Precedence: env > user store > auth files. The file still holds
	// user_omp here; a saved store must win over it.
	if err := config.SaveUserAPIKeyFor(config.ProviderCommandCode, "sk-store"); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.GetAPIKeyFor(config.ProviderCommandCode); got != "sk-store" {
		t.Fatalf("store precedence: %q", got)
	}
	t.Setenv("CMD_API_KEY", "sk-env")
	if got, _ := config.GetAPIKeyFor(config.ProviderCommandCode); got != "sk-env" {
		t.Fatalf("env precedence: %q", got)
	}

	// Malformed files are skipped without breaking the chain.
	t.Setenv("CMD_API_KEY", "")
	os.Remove(filepath.Join(home, ".omp", "agent", "auth.json"))
	writeHome(filepath.Join(".commandcode", "auth.json"), "{not json")
	if _, err := config.GetAPIKeyFor(config.ProviderCommandCode); err != nil {
		t.Fatalf("malformed file must be skipped, got %v (store should have answered)", err)
	}
}

func TestOpencodeGoCatalogEntries(t *testing.T) {
	// ox-alpha-free: $0 but served from the PAID endpoint — it must appear
	// under the go plan's list, never the keyless free tier's.
	if base := config.ModelBaseURL("ox-alpha-free"); base != config.BaseURL {
		t.Fatalf("ox-alpha-free route: %s", base)
	}
	if config.FreeTierModel("ox-alpha-free") {
		t.Fatal("ox-alpha-free must not count as free-tier (it rides zen/go)")
	}
	if in, out := config.ModelRates("ox-alpha-free"); in != 0 || out != 0 {
		t.Fatalf("ox-alpha-free rates: %v %v", in, out)
	}
	// The other go-route additions resolve and keep ascending order intact.
	for _, id := range []string{"glm-5.3", "muse-spark-1.2-contributor", "deepseek-v4-flash-vision-exp"} {
		found := false
		for _, m := range config.Models {
			if m.ID == id {
				found = true
				if m.BaseURL != "" {
					t.Fatalf("%s must ride the paid default route", id)
				}
			}
		}
		if !found {
			t.Fatalf("missing go-route model: %s", id)
		}
	}
}

func TestOpencodeCLIAuthFileChain(t *testing.T) {
	setupXDG(t)
	t.Setenv("OPENCODE_API_KEY", "")
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)

	// Nothing anywhere → error names the env var.
	if _, err := config.GetAPIKeyFor(config.ProviderOpencode); err == nil {
		t.Fatal("want error with no sources")
	}

	// The opencode CLI's login store shape.
	path := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(
		`{"opencode-go":{"type":"api","key":"user_go"},"other":{"type":"api","key":"x"}}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := config.GetAPIKeyFor(config.ProviderOpencode)
	if err != nil || got != "user_go" {
		t.Fatalf("cli auth file: %q %v", got, err)
	}

	// Precedence: our own user store still wins over the CLI file.
	if err := config.SaveUserAPIKey("sk-store"); err != nil {
		t.Fatal(err)
	}
	if got, _ = config.GetAPIKeyFor(config.ProviderOpencode); got != "sk-store" {
		t.Fatalf("store precedence: %q", got)
	}

	// Malformed CLI file is skipped, not fatal.
	os.Remove(path)
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ = config.GetAPIKeyFor(config.ProviderOpencode); got != "sk-store" {
		t.Fatalf("malformed file handling: %q", got)
	}
}
