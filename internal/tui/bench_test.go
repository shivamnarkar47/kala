package tui

import (
	"strings"
	"testing"
)

func TestBenchModelProgressAndReportCard(t *testing.T) {
	m := newBenchModel(BenchConfig{
		BaseURL: "https://bench.test/v1", Model: "test-model",
		Prompt: "hi", Requests: 3, MaxTokens: 20, TimeoutSec: 5,
	}, nil)
	m.width = 100

	// Mid-run: progress bar + per-request rows, no report yet.
	for i := 0; i < 2; i++ {
		out, _ := m.Update(benchResultMsg{res: benchResult{
			index: i, ttft: 0.5 + float64(i)*0.1, total: 1.0 + float64(i)*0.1, chars: 40,
		}})
		m = out.(*benchModel)
	}
	view := m.View()
	if !strings.Contains(view, "kaal bench") || !strings.Contains(view, "2/3") {
		t.Fatalf("progress view missing: %q", tailOf(view, 300))
	}
	if strings.Contains(view, "p50") {
		t.Fatal("report card must not render before the run completes")
	}

	// Final result flips to the report card with percentiles.
	out, cmd := m.Update(benchResultMsg{res: benchResult{index: 2, ttft: 0.7, total: 1.4, chars: 50}})
	if cmd != nil {
		t.Fatal("last request must not schedule another")
	}
	m = out.(*benchModel)
	if !m.done {
		t.Fatal("run must be done after the final result")
	}
	view = plain(m.View())
	for _, want := range []string{"TTFT", "TOTAL", "p50", "p99", "3 ok", "0 failed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("report card missing %q in %q", want, tailOf(view, 400))
		}
	}
}

func TestBenchModelFailureRowAndExitPath(t *testing.T) {
	m := newBenchModel(BenchConfig{BaseURL: "https://x/v1", Model: "m",
		Requests: 2, MaxTokens: 10, TimeoutSec: 5}, nil)
	m.width = 100
	out, _ := m.Update(benchResultMsg{res: benchResult{index: 0, err: "HTTP 401: nope"}})
	m = out.(*benchModel)
	if !strings.Contains(plain(m.View()), "FAILED") {
		t.Fatal("failure row missing")
	}
}
