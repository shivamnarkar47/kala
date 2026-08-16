// Package config ports harness/config.py: constants, the model catalog, and
// API-key resolution for the opencode-go gateway.
//
// API-key resolution order: env OPENCODE_API_KEY → user key store
// (user_key_path(), written by the TUI's /connect) → omp auth store
// (~/.omp/agent/agent.db, read-only sqlite via modernc.org/sqlite — pure Go,
// no cgo) → error with instructions.
package config

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Gateway endpoints.
const (
	BaseURL         = "https://opencode.ai/zen/go/v1"
	ChatCompletions = BaseURL + "/chat/completions"
	// FreeBaseURL: the opencode free tier lives on the zen/v1 endpoint
	// (same OPENCODE_API_KEY); catalog models that list this base_url route
	// there.
	FreeBaseURL = "https://opencode.ai/zen/v1"
	ModelID     = "deepseek-v4-flash"
)

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

// Models is the model catalog — refreshed against the local opencode cache.
// Sorted free first, then ascending by price. deepseek-v4-flash is the
// shipped default.
var Models = []Model{
	// -- opencode free tier (zen/v1, $0) --
	{ID: "deepseek-v4-flash-free", Name: "DeepSeek V4 Flash (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "qwen3.6-plus-free", Name: "Qwen3.6 Plus (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "kimi-k2.5-free", Name: "Kimi K2.5 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "glm-5-free", Name: "GLM-5 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "hy3-free", Name: "Hy3 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "minimax-m2.5-free", Name: "MiniMax-M2.5 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "mimo-v2.5-free", Name: "MiMo V2.5 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "mimo-v2-pro-free", Name: "MiMo V2 Pro (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "minimax-m3-free", Name: "MiniMax-M3 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "glm-4.7-free", Name: "GLM-4.7 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "nemotron-3-super-free", Name: "Nemotron 3 Super (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "north-mini-code-free", Name: "North Mini Code (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "trinity-large-preview-free", Name: "Trinity Large Preview (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "hy3-preview-free", Name: "Hy3 Preview (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "ling-3.0-flash-free", Name: "Ling-3.0 Flash (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "laguna-s-2.1-free", Name: "Laguna S 2.1 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "mimo-v2-omni-free", Name: "MiMo V2 Omni (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "minimax-m2.1-free", Name: "MiniMax-M2.1 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "mimo-v2-flash-free", Name: "MiMo V2 Flash (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "grok-code", Name: "Grok Code Fast 1 (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "big-pickle", Name: "Big Pickle (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "nemotron-3-ultra-free", Name: "Nemotron 3 Ultra (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "ring-2.6-1t-free", Name: "Ring 2.6 1T (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	{ID: "ling-2.6-flash-free", Name: "Ling 2.6 Flash (Free)", InputPerM: 0, OutputPerM: 0, BaseURL: FreeBaseURL},
	// -- paid (opencode-go), ascending --
	{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", InputPerM: 0.1, OutputPerM: 0.6},
	{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", InputPerM: 0.14, OutputPerM: 0.28},
	{ID: "mimo-v2.5", Name: "MiMo V2.5", InputPerM: 0.14, OutputPerM: 0.28},
	{ID: "hy3", Name: "Hy3", InputPerM: 0.14, OutputPerM: 0.58},
	{ID: "qwen3.5-plus", Name: "Qwen3.5 Plus", InputPerM: 0.2, OutputPerM: 1.2},
	{ID: "minimax-m2.5", Name: "MiniMax-M2.5", InputPerM: 0.3, OutputPerM: 1.2},
	{ID: "minimax-m2.7", Name: "MiniMax-M2.7", InputPerM: 0.3, OutputPerM: 1.2},
	{ID: "minimax-m3", Name: "MiniMax-M3", InputPerM: 0.3, OutputPerM: 1.2},
	{ID: "qwen3.7-plus", Name: "Qwen3.7 Plus", InputPerM: 0.4, OutputPerM: 1.6},
	{ID: "mimo-v2-omni", Name: "MiMo V2 Omni", InputPerM: 0.4, OutputPerM: 2.0},
	{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", InputPerM: 0.435, OutputPerM: 0.87},
	{ID: "mimo-v2.5-pro", Name: "MiMo V2.5 Pro", InputPerM: 0.435, OutputPerM: 0.87},
	{ID: "qwen3.6-plus", Name: "Qwen3.6 Plus", InputPerM: 0.5, OutputPerM: 3.0},
	{ID: "kimi-k2.5", Name: "Kimi K2.5", InputPerM: 0.6, OutputPerM: 3.0},
	{ID: "kimi-k2.6", Name: "Kimi K2.6", InputPerM: 0.95, OutputPerM: 4.0},
	{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", InputPerM: 0.95, OutputPerM: 4.0},
	{ID: "mimo-v2-pro", Name: "MiMo V2 Pro", InputPerM: 1.0, OutputPerM: 3.0},
	{ID: "glm-5", Name: "GLM-5", InputPerM: 1.0, OutputPerM: 3.2},
	{ID: "glm-5.1", Name: "GLM-5.1", InputPerM: 1.4, OutputPerM: 4.4},
	{ID: "glm-5.2", Name: "GLM-5.2", InputPerM: 1.4, OutputPerM: 4.4},
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

// ModelBaseURL returns the gateway endpoint for a model: free-tier entries
// route to FreeBaseURL, everything else to BaseURL.
func ModelBaseURL(modelID string) string {
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

// userKeyPath is $XDG_CONFIG_HOME/kaal/api_key (POSIX).
func userKeyPath() string {
	return filepath.Join(xdgConfigHome(), "kaal", "api_key")
}

// SaveUserAPIKey persists the user's API key: raw text, no trailing newline,
// 0600 on POSIX.
func SaveUserAPIKey(key string) error {
	path := userKeyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// LoadUserAPIKey returns the stored user key, or "". Never printed or cached.
func LoadUserAPIKey() string {
	raw, err := os.ReadFile(userKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

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

// GetAPIKey resolves the gateway API key: env, then the user key store, then
// omp's auth store, then an error with instructions. Never writes or caches
// the key.
func GetAPIKey() (string, error) {
	if envKey := os.Getenv("OPENCODE_API_KEY"); envKey != "" {
		return envKey, nil
	}
	if userKey := LoadUserAPIKey(); userKey != "" {
		return userKey, nil
	}
	if ompKey := ompAPIKey(); ompKey != "" {
		return ompKey, nil
	}
	return "", fmt.Errorf(
		"kaal: no API key found. Set OPENCODE_API_KEY, run `kaal` and use "+
			"/connect, or re-add the opencode-go credential (`opencode` / "+
			"`omp /connect`). Checked: env OPENCODE_API_KEY, %s, and "+
			"~/.omp/agent/agent.db.", userKeyPath())
}
