// Package config ports harness/config.py: constants, the model catalog, and
// API-key resolution for two gateway providers.
//
// Key resolution per provider:
//
//	opencode:    env OPENCODE_API_KEY → user key store (user_key_path(),
//	             written by the TUI's /connect) → omp auth store
//	             (~/.omp/agent/agent.db, read-only sqlite via
//	             modernc.org/sqlite — pure Go, no cgo) → error.
//	commandcode: env CMD_API_KEY (or COMMANDCODE_API_KEY) → user key store
//	             (api_key.commandcode) → error.
package config

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Gateway endpoints.
const (
	BaseURL         = "https://opencode.ai/zen/go/v1"
	ChatCompletions = BaseURL + "/chat/completions"
	// FreeBaseURL: the opencode free tier lives on the zen/v1 endpoint
	// (same OPENCODE_API_KEY); catalog models that list this base_url route
	// there. It is the shipped default route.
	FreeBaseURL = "https://opencode.ai/zen/v1"
	// CommandCodeBaseURL: Command Code's Provider API
	// (https://commandcode.ai/docs/provider) — OpenAI chat-completions
	// compatible, Bearer auth, own key (CMD_API_KEY). Anthropic-family
	// models there need the /messages endpoint, so only OpenAI-shaped
	// models join the catalog.
	CommandCodeBaseURL = "https://api.commandcode.ai/provider/v1"
	// ModelID is the shipped default: Hy3 on opencode's keyless free tier
	// (zen/v1 answers anonymous requests for *-free models — no login at
	// all). deepseek-v4-flash-free stays in the catalog for when its
	// upstream recovers; /model switches anytime.
	ModelID = "hy3-free"
)

// Providers behind the catalog. The provider decides which endpoint a model
// routes to and which key chain resolves.
const (
	ProviderOpencode    = "opencode"
	ProviderCommandCode = "commandcode"
)

// FreeTierModel reports whether a model rides zen/v1's keyless free tier:
// those models need no OPENCODE_API_KEY whatsoever (anonymous requests are
// accepted; dummy bearer tokens, ironically, are rejected).
func FreeTierModel(modelID string) bool {
	return ModelBaseURL(modelID) == FreeBaseURL
}

// ModelProvider resolves the provider serving a model id: a stored custom
// provider wins when it owns the id, then the built-in catalog routes;
// unknown ids ride the main opencode gateway.
func ModelProvider(modelID string) string {
	if cp := LoadCustomProvider(); cp != nil && modelID == cp.Model && cp.BaseURL != "" {
		return cp.Name
	}
	if ModelBaseURL(modelID) == CommandCodeBaseURL {
		return ProviderCommandCode
	}
	return ProviderOpencode
}

// Model limits (catalog: contextWindow / maxTokens). MaxOutputTokens is
// deliberately capped well below the catalog's 384k: an unbounded output
// budget lets the model's reasoning run away.
const (
	ContextWindow   = 1_000_000
	MaxOutputTokens = 32_000
)

// ReasoningField is the delta field that carries streamed reasoning.
const ReasoningField = "reasoning_content"

// RequestTimeout is the request timeout per HTTP request, seconds.
const RequestTimeout = 120

// Catalog pricing (verified against @oh-my-pi/pi-catalog models.json, the
// opencode-go/deepseek-v4-flash compat block): input $0.14/M, output
// $0.28/M, cache-read $0.0028/M.
const (
	InputCostPerM     = 0.14
	OutputCostPerM    = 0.28
	CacheReadCostPerM = 0.0028
)

// Model is one catalog entry.
type Model struct {
	ID         string
	Name       string
	InputPerM  float64
	OutputPerM float64
	BaseURL    string // empty = paid route (BaseURL)
}

// Models is the model catalog. Sorted free first, then ascending by input
// price. deepseek-v4-flash-free is the shipped default. Model ids are unique
// across providers: when both gateways serve the same plain id (minimax-m3,
// mimo-v2.5, laguna-s-2.1-free…), the opencode entry wins and Command Code
// is reached through its namespaced form (stealth/ox-alpha,
// deepseek/deepseek-v4-flash).
var Models = []Model{
	// -- opencode free tier (zen/v1, $0) --
	{ID: "deepseek-v4-flash-free", Name: "DeepSeek V4 Flash (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "qwen3.6-plus-free", Name: "Qwen3.6 Plus (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "kimi-k2.5-free", Name: "Kimi K2.5 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "glm-5-free", Name: "GLM-5 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "hy3-free", Name: "Hy3 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "x-preview-f-free", Name: "X Preview F (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "minimax-m2.5-free", Name: "MiniMax-M2.5 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "mimo-v2.5-free", Name: "MiMo V2.5 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "mimo-v2-pro-free", Name: "MiMo V2 Pro (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "minimax-m3-free", Name: "MiniMax-M3 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "glm-4.7-free", Name: "GLM-4.7 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "nemotron-3-super-free", Name: "Nemotron 3 Super (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "nemotron-3.5-lightning-free", Name: "Nemotron 3.5 Lightning (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "north-mini-code-free", Name: "North Mini Code (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "trinity-large-preview-free", Name: "Trinity Large Preview (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "hy3-preview-free", Name: "Hy3 Preview (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "ling-3.0-flash-free", Name: "Ling-3.0 Flash (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "ling-3.0-tiny-free", Name: "Ling-3.0 Tiny (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "laguna-s-2.1-free", Name: "Laguna S 2.1 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "mimo-v2-omni-free", Name: "MiMo V2 Omni (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "minimax-m2.1-free", Name: "MiniMax-M2.1 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "mimo-v2-flash-free", Name: "MiMo V2 Flash (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "grok-code", Name: "Grok Code Fast 1 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "big-pickle", Name: "Big Pickle (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "nemotron-3-ultra-free", Name: "Nemotron 3 Ultra (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "ring-2.6-1t-free", Name: "Ring 2.6 1T (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "ling-2.6-flash-free", Name: "Ling 2.6 Flash (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "longcat-2.0-free", Name: "LongCat 2.0 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	// Go-route promo: $0 but served from the PAID endpoint (zen/go/v1) —
	// keyless like the free tier, visible only under the go plan's list.
	{ID: "ox-alpha-free", Name: "Ox Alpha (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: BaseURL},
	// -- Command Code free (api.commandcode.ai, $0; CMD_API_KEY) --
	// Rates verified against commandcode.ai/docs/resources/pricing-limits.
	{ID: "stealth/ox-alpha", Name: "Ox Alpha (Command Code, stealth preview)", InputPerM: 0, OutputPerM: 0, BaseURL: CommandCodeBaseURL},
	{ID: "ling-3.0-flash", Name: "Ling 3.0 Flash (Command Code, free)", InputPerM: 0, OutputPerM: 0, BaseURL: CommandCodeBaseURL},
	// -- paid (opencode-go), ascending --
	{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", InputPerM: 0.1, OutputPerM: 0.6},
	{ID: "muse-spark-1.2-contributor", Name: "Muse Spark 1.2 Contributor", InputPerM: 0.1, OutputPerM: 0.2},
	{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", InputPerM: 0.14, OutputPerM: 0.28},
	{ID: "deepseek-v4-flash-vision-exp", Name: "DeepSeek V4 Flash Vision (Exp)", InputPerM: 0.14, OutputPerM: 0.28},
	{ID: "mimo-v2.5", Name: "MiMo V2.5", InputPerM: 0.14, OutputPerM: 0.28},
	{ID: "hy3", Name: "Hy3", InputPerM: 0.14, OutputPerM: 0.58},
	{ID: "qwen3.5-plus", Name: "Qwen3.5 Plus", InputPerM: 0.2, OutputPerM: 1.2},
	// -- Command Code paid (api.commandcode.ai; CMD_API_KEY) --
	{ID: "deepseek/deepseek-v4-flash", Name: "DeepSeek V4 Flash (Command Code)", InputPerM: 0.22, OutputPerM: 0.66, BaseURL: CommandCodeBaseURL},
	{ID: "minimax-m2.5", Name: "MiniMax-M2.5", InputPerM: 0.3, OutputPerM: 1.2},
	{ID: "minimax-m2.7", Name: "MiniMax-M2.7", InputPerM: 0.3, OutputPerM: 1.2},
	{ID: "minimax-m3", Name: "MiniMax-M3", InputPerM: 0.3, OutputPerM: 1.2},
	{ID: "qwen3.7-plus", Name: "Qwen3.7 Plus", InputPerM: 0.4, OutputPerM: 1.6},
	{ID: "mimo-v2-omni", Name: "MiMo V2 Omni", InputPerM: 0.4, OutputPerM: 2.0},
	{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", InputPerM: 0.435, OutputPerM: 0.87},
	{ID: "mimo-v2.5-pro", Name: "MiMo V2.5 Pro", InputPerM: 0.435, OutputPerM: 0.87},
	{ID: "qwen3.6-plus", Name: "Qwen3.6 Plus", InputPerM: 0.5, OutputPerM: 3.0},
	{ID: "kimi-k2.5", Name: "Kimi K2.5", InputPerM: 0.6, OutputPerM: 3.0},
	{ID: "google/gemini-3.7-flash", Name: "Gemini 3.7 Flash (Command Code, deal)", InputPerM: 0.75, OutputPerM: 3.75, BaseURL: CommandCodeBaseURL},
	{ID: "kimi-k2.6", Name: "Kimi K2.6", InputPerM: 0.95, OutputPerM: 4.0},
	{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", InputPerM: 0.95, OutputPerM: 4.0},
	{ID: "mimo-v2-pro", Name: "MiMo V2 Pro", InputPerM: 1.0, OutputPerM: 3.0},
	{ID: "glm-5", Name: "GLM-5", InputPerM: 1.0, OutputPerM: 3.2},
	{ID: "glm-5.1", Name: "GLM-5.1", InputPerM: 1.4, OutputPerM: 4.4},
	{ID: "glm-5.2", Name: "GLM-5.2", InputPerM: 1.4, OutputPerM: 4.4},
	{ID: "glm-5.3", Name: "GLM-5.3", InputPerM: 1.4, OutputPerM: 4.4},
	{ID: "qwen3.8-max", Name: "Qwen3.8 Max", InputPerM: 2.0, OutputPerM: 6.0},
	{ID: "grok-4.5", Name: "Grok 4.5", InputPerM: 2.0, OutputPerM: 6.0},
	{ID: "qwen3.7-max", Name: "Qwen3.7 Max", InputPerM: 2.5, OutputPerM: 7.5},
	{ID: "kimi-k3", Name: "Kimi K3", InputPerM: 3.0, OutputPerM: 15.0},
}

// ModelRates returns (input_per_m, output_per_m) for a catalog model; falls
// back to the deepseek-v4-flash rates for unknown ids.
func ModelRates(modelID string) (float64, float64) {
	for _, m := range Models {
		if m.ID == modelID {
			return m.InputPerM, m.OutputPerM
		}
	}
	return 0.14, 0.28
}

// ModelBaseURL returns the gateway endpoint for a model: a stored custom
// provider wins when it owns the id, free-tier entries route to FreeBaseURL,
// Command Code entries to its endpoint, everything else to BaseURL.
func ModelBaseURL(modelID string) string {
	if cp := LoadCustomProvider(); cp != nil && modelID == cp.Model && cp.BaseURL != "" {
		return strings.TrimRight(cp.BaseURL, "/")
	}
	for _, m := range Models {
		if m.ID == modelID && m.BaseURL != "" {
			return m.BaseURL
		}
	}
	return BaseURL
}

// EstimateCost returns the estimated dollar cost for a token mix (per-1M
// catalog rates). modelID uses that model's own rates; "" keeps the
// deepseek-v4-flash defaults. Rounded to 6 decimals.
func EstimateCost(inputTokens, outputTokens, cacheReadTokens int, modelID string) float64 {
	inputPerM, outputPerM := ModelRates(modelID)
	cost := float64(inputTokens)/1_000_000*inputPerM +
		float64(outputTokens)/1_000_000*outputPerM +
		float64(cacheReadTokens)/1_000_000*CacheReadCostPerM
	return round6(cost)
}

func round6(f float64) float64 {
	return float64(int64(f*1_000_000+0.5)) / 1_000_000
}

// -- user model preference ----------------------------------------------------

// userModelPath is $XDG_CONFIG_HOME/kaal/model.
func userModelPath() string {
	return filepath.Join(xdgConfigHome(), "kaal", "model")
}

// SaveUserModel persists the default model id: raw text, no trailing newline.
func SaveUserModel(modelID string) error {
	path := userModelPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(modelID), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadUserModel returns the stored default model id, or "".
func LoadUserModel() string {
	raw, err := os.ReadFile(userModelPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// ResolveModelID applies the selection order: --model flag > saved default >
// ModelID.
func ResolveModelID(flag string) string {
	if flag != "" {
		return flag
	}
	if saved := LoadUserModel(); saved != "" {
		return saved
	}
	return ModelID
}

// -- user API key --------------------------------------------------------------

// userKeyPathFor is $XDG_CONFIG_HOME/kaal/api_key (opencode) or
// api_key.commandcode (Command Code). One file per provider; never shared.
func userKeyPathFor(provider string) string {
	if provider == ProviderCommandCode {
		return filepath.Join(xdgConfigHome(), "kaal", "api_key.commandcode")
	}
	return filepath.Join(xdgConfigHome(), "kaal", "api_key")
}

// userKeyPath is the opencode key store path.
func userKeyPath() string { return userKeyPathFor(ProviderOpencode) }

// SaveUserAPIKey persists an API key in the provider's store: raw text, no
// trailing newline, 0600 on POSIX. This is the only sanctioned writer.
func SaveUserAPIKeyFor(provider, key string) error {
	path := userKeyPathFor(provider)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// SaveUserAPIKey persists the opencode key (the /connect default).
func SaveUserAPIKey(key string) error { return SaveUserAPIKeyFor(ProviderOpencode, key) }

// LoadUserAPIKeyFor returns the stored provider key, or "". Never printed or
// cached.
func LoadUserAPIKeyFor(provider string) string {
	raw, err := os.ReadFile(userKeyPathFor(provider))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// LoadUserAPIKey returns the stored opencode key, or "".
func LoadUserAPIKey() string { return LoadUserAPIKeyFor(ProviderOpencode) }

func xdgConfigHome() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return base
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config"
	}
	return filepath.Join(home, ".config")
}

// ompAPIKey reads the omp auth store (~/.omp/agent/agent.db, read-only
// sqlite). Pure-Go driver (modernc.org/sqlite) per the migration plan; any
// failure degrades to "" so the fallback order holds.
func ompAPIKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dbPath := filepath.Join(home, ".omp", "agent", "agent.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return ""
	}
	defer db.Close()
	row := db.QueryRow(
		"SELECT data FROM auth_credentials WHERE provider = 'opencode-go' AND credential_type = 'api_key'",
	)
	var data string
	if err := row.Scan(&data); err != nil {
		return ""
	}
	var payload struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return ""
	}
	return payload.Key
}

// GetAPIKeyFor resolves the gateway API key for a provider: the provider's
// env var, then its user key store, then (opencode only) omp's auth store —
// else an error with instructions. Custom providers resolve from their env
// var (<NAME>_API_KEY) and the stored BYOK config. Never writes or caches
// the key.
//
//	opencode    → OPENCODE_API_KEY → api_key → ~/.omp
//	commandcode → CMD_API_KEY | COMMANDCODE_API_KEY → api_key.commandcode →
//	              shared auth files (~/.commandcode/auth.json, …)
//	<custom>    → <NAME>_API_KEY → custom_provider.json
func GetAPIKeyFor(provider string) (string, error) {
	envNames := []string{"OPENCODE_API_KEY"}
	userStore := func() string { return LoadUserAPIKeyFor(provider) }
	switch {
	case provider == ProviderCommandCode:
		envNames = []string{"CMD_API_KEY", "COMMANDCODE_API_KEY"}
	case provider != ProviderOpencode:
		if cp := LoadCustomProvider(); cp != nil && cp.Name == provider {
			envNames = []string{CustomEnvName(provider)}
			if envKey := os.Getenv(envNames[0]); envKey != "" {
				return envKey, nil
			}
			if cp.APIKey != "" {
				return cp.APIKey, nil
			}
			return "", fmt.Errorf(
				"kaal: no API key found for %s. Set %s or re-run /connect → "+
					"add another provider. Checked: env %s and %s.",
				provider, envNames[0], envNames[0], customProviderPath())
		}
	}
	for _, name := range envNames {
		if envKey := os.Getenv(name); envKey != "" {
			return envKey, nil
		}
	}
	if userKey := userStore(); userKey != "" {
		return userKey, nil
	}
	// Command Code: existing CLI/pi/OMP logins live in shared auth files —
	// a Go-plan key there works without re-entering anything.
	if provider == ProviderCommandCode {
		if fileKey := commandCodeAuthFileKey(); fileKey != "" {
			return fileKey, nil
		}
	}
	// opencode: the opencode CLI's own login store carries provider keys
	// ({"opencode-go": {"type": "api", "key": …}}) — same account, no
	// re-entry needed.
	if provider == ProviderOpencode {
		if fileKey := opencodeCLIAuthFileKey(); fileKey != "" {
			return fileKey, nil
		}
	}
	if provider == ProviderOpencode {
		if ompKey := ompAPIKey(); ompKey != "" {
			return ompKey, nil
		}
	}
	article := "a"
	if provider == ProviderOpencode {
		article = "an"
	}
	hint := ""
	if provider == ProviderCommandCode {
		hint = ", or log in once with the command-code CLI (~/.commandcode/auth.json)"
	}
	return "", fmt.Errorf(
		"kaal: no API key found for %s. Set %s, run `kaal` and use /connect "+
			"while %s %s model is active%s%s. Checked: env %s and %s.",
		provider, strings.Join(envNames, " or "), article, provider,
		map[bool]string{true: ", or re-add the opencode-go credential (`opencode` / `omp /connect`)", false: ""}[provider == ProviderOpencode],
		hint,
		strings.Join(envNames, "/"), userKeyPathFor(provider))
}

// GetAPIKey resolves the opencode gateway key (env → user store → omp).
func GetAPIKey() (string, error) { return GetAPIKeyFor(ProviderOpencode) }

// -- custom provider (BYOK) -----------------------------------------------------

// CustomProvider is a user-added OpenAI-compatible endpoint (the TUI's
// /connect → "add another provider"): a base URL, the stored key, and the
// chosen model. When set, it owns its model id for every routing lookup.
type CustomProvider struct {
	Name    string `json:"name"` // derived from the host, e.g. "together"
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

var (
	customMu     sync.Mutex
	customCache  *CustomProvider
	customLoaded bool
)

func customProviderPath() string {
	return filepath.Join(xdgConfigHome(), "kaal", "custom_provider.json")
}

// SaveCustomProvider persists the custom provider atomically; the file holds
// an API key, so 0600. This is the only sanctioned writer.
func SaveCustomProvider(cp CustomProvider) error {
	cp.Name = sanitizeProviderName(cp.Name)
	path := customProviderPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	customMu.Lock()
	customCache = &cp
	customLoaded = true
	customMu.Unlock()
	return nil
}

// LoadCustomProvider returns the stored custom provider, or nil when absent
// or corrupt. Cached per process; SaveCustomProvider refreshes the cache.
func LoadCustomProvider() *CustomProvider {
	customMu.Lock()
	defer customMu.Unlock()
	if customLoaded {
		return customCache
	}
	customLoaded = true
	raw, err := os.ReadFile(customProviderPath())
	if err != nil {
		return nil
	}
	var cp CustomProvider
	if json.Unmarshal(raw, &cp) != nil || cp.BaseURL == "" || cp.Model == "" {
		return nil
	}
	customCache = &cp
	return customCache
}

// ClearCustomProvider removes the stored custom provider (back to built-ins).
func ClearCustomProvider() error {
	customMu.Lock()
	customCache = nil
	customLoaded = true
	customMu.Unlock()
	err := os.Remove(customProviderPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// sanitizeProviderName keeps provider names filesystem/env safe: lowercase,
// [a-z0-9-] only, non-empty.
func sanitizeProviderName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	out := strings.Trim(sb.String(), "-")
	if out == "" {
		out = "custom"
	}
	return out
}

// CustomEnvName derives the env var that can carry a custom provider's key:
// provider "together" → TOGETHER_API_KEY.
func CustomEnvName(provider string) string {
	up := strings.ToUpper(sanitizeProviderName(provider))
	up = strings.ReplaceAll(up, "-", "_")
	return up + "_API_KEY"
}

// commandCodeAuthFiles are the auth files the official Command Code CLI and
// the pi/OMP integrations write; kaal reads them read-only, first hit wins.
func commandCodeAuthFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".commandcode", "auth.json"),
		filepath.Join(home, ".omp", "agent", "auth.json"),
		filepath.Join(home, ".pi", "agent", "auth.json"),
	}
}

// commandCodeAuthFileKey extracts a Command Code key from the shared auth
// files. Accepted shapes per file: {"apiKey": "user_…"},
// {"commandcode": "user_…"}, {"command-code": "user_…"}, or any of those
// fields holding a credential record {"type":"api","key":…} /
// {"type":"oauth","access":…}. Unreadable/malformed files are skipped.
func commandCodeAuthFileKey() string {
	for _, path := range commandCodeAuthFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil || doc == nil {
			continue
		}
		if s, ok := doc["apiKey"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		for _, field := range []string{"commandcode", "command-code"} {
			switch v := doc[field].(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			case map[string]any:
				if cred := credentialKey(v); cred != "" {
					return cred
				}
			}
		}
	}
	return ""
}

// credentialKey pulls a key out of a stored credential record: api → key,
// oauth → access, unknown shapes fall back to whichever string is present.
func credentialKey(cred map[string]any) string {
	credType, _ := cred["type"].(string)
	if credType == "oauth" {
		if access, _ := cred["access"].(string); access != "" {
			return access
		}
	}
	if key, _ := cred["key"].(string); key != "" && (credType == "api" || credType == "") {
		return key
	}
	if access, _ := cred["access"].(string); access != "" {
		return access
	}
	if key, _ := cred["key"].(string); key != "" {
		return key
	}
	return ""
}

// opencodeCLIAuthFileKey reads the opencode CLI's own login store
// (~/.local/share/opencode/auth.json): provider-keyed credentials like
// {"opencode-go": {"type": "api", "key": …}}. Read-only; unreadable or
// malformed files are skipped.
func opencodeCLIAuthFileKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".local", "share", "opencode", "auth.json"))
	if err != nil {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return ""
	}
	for _, field := range []string{"opencode-go", "opencode"} {
		switch v := doc[field].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]any:
			if cred := credentialKey(v); cred != "" {
				return cred
			}
		}
	}
	return ""
}
