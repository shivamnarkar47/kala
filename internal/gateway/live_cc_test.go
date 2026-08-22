// Live Command Code /alpha/generate diagnostics. Skipped unless
// KAAL_LIVE_CC=1 — hits the real endpoint with the machine's stored key and
// bills its plan credits (tiny prompts only).
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/kaal/kaal/internal/config"
	"github.com/kaal/kaal/internal/messages"
)

func liveCCPost(t *testing.T, payload []byte) (int, string) {
	t.Helper()
	key, err := config.GetAPIKeyFor(config.ProviderCommandCode)
	if err != nil {
		t.Fatalf("no commandcode key: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.commandcode.ai/alpha/generate", bytes.NewReader(payload))
	h := buildHeaders(key)
	h.Set("x-command-code-version", commandCodeCLIVersion)
	h.Set("x-cli-environment", "production")
	req.Header = h
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body)
}

func TestLiveCCGenerateRoundTrip(t *testing.T) {
	if os.Getenv("KAAL_LIVE_CC") != "1" {
		t.Skip("set KAAL_LIVE_CC=1 to bill real credits against the Go-plan endpoint")
	}
	model := os.Getenv("KAAL_LIVE_CC_MODEL")
	if model == "" {
		model = "deepseek/deepseek-v4-flash"
	}

	// Turn 2 shape: assistant tool-call + tool result replay.
	msgs := []any{
		messages.WireSystem{Role: "system", Content: "You are kaal."},
		messages.WireUser{Role: "user", Content: "Read hello.txt using the read_file tool."},
		messages.WireAssistant{
			Role:    "assistant",
			Content: "",
			ToolCalls: []messages.WireToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: messages.WireFunction{
					Name:      "read_file",
					Arguments: `{"path":"hello.txt"}`,
				},
			}},
		},
		messages.WireToolResult{Role: "tool", ToolCallID: "call_1", Content: "hello world"},
	}
	payload, err := buildGenerateBody(model, msgs, []any{}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	status, body := liveCCPost(t, payload)
	fmt.Printf("=== ROUND-TRIP status=%d\n%s\n", status, body)
	if status == 400 {
		// Dump the exact payload so the failing field can be bisected.
		var pretty map[string]any
		json.Unmarshal(payload, &pretty)
		out, _ := json.MarshalIndent(pretty, "", "  ")
		t.Logf("payload:\n%s", out)
	}
}
