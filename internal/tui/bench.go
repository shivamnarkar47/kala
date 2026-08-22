// Package tui — bench.go: the `kaal bench` surface. A small bubbletea
// program that fires sequential streaming requests at an OpenAI-compatible
// endpoint, shows live progress, and finishes with a percentile report card.
package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// BenchConfig drives one benchmark run.
type BenchConfig struct {
	BaseURL    string // e.g. https://opencode.ai/zen/v1 (no trailing slash)
	Model      string
	APIKey     string // empty = keyless
	Prompt     string
	Requests   int
	MaxTokens  int
	TimeoutSec int
}

// benchResult is one completed request.
type benchResult struct {
	index int
	ttft  float64 // seconds; -1 when nothing streamed
	total float64
	chars int
	err   string
}

type benchModel struct {
	cfg      BenchConfig
	send     func(tea.Msg)
	results  []benchResult
	inflight bool
	done     bool
	aborted  bool
	cancel   context.CancelFunc
	width    int
}

type benchResultMsg struct{ res benchResult }

type benchQuitMsg struct{}

func newBenchModel(cfg BenchConfig, cancel context.CancelFunc) *benchModel {
	return &benchModel{cfg: cfg, cancel: cancel}
}

// benchRequest performs one timed streaming completion (shared by the TUI
// and the --json mode).
func benchRequest(ctx context.Context, cfg BenchConfig, index int) benchResult {
	res := benchResult{index: index, ttft: -1}
	body, _ := json.Marshal(map[string]any{
		"model":      cfg.Model,
		"messages":   []map[string]any{{"role": "user", "content": cfg.Prompt}},
		"max_tokens": cfg.MaxTokens,
		"stream":     true,
	})
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.BaseURL, "/")+"/chat/completions",
		strings.NewReader(string(body)))
	if err != nil {
		res.err = err.Error()
		return res
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "python-requests/2.31.0")

	t0 := time.Now()
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		res.err = err.Error()
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		res.total = time.Since(t0).Seconds()
		res.err = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		return res
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 32*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		text, _ := delta["content"].(string)
		if reasoning, ok := delta["reasoning_content"].(string); ok && text == "" {
			text = reasoning
		}
		if text != "" {
			if res.ttft < 0 {
				res.ttft = time.Since(t0).Seconds()
			}
			res.chars += len(text)
		}
	}
	res.total = time.Since(t0).Seconds()
	return res
}

func (b *benchModel) requestCmd(index int) tea.Cmd {
	cfg := b.cfg
	return func() tea.Msg {
		return benchResultMsg{res: benchRequest(context.Background(), cfg, index)}
	}
}

func (b *benchModel) Init() tea.Cmd { return b.requestCmd(0) }

func (b *benchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		b.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			b.aborted = true
			if b.cancel != nil {
				b.cancel()
			}
			return b, tea.Quit
		case "q", "esc":
			if b.done {
				return b, tea.Quit
			}
		}
	case benchResultMsg:
		b.results = append(b.results, msg.res)
		b.inflight = false
		next := len(b.results)
		if next >= b.cfg.Requests {
			b.done = true
			return b, nil
		}
		return b, b.requestCmd(next)
	}
	return b, nil
}

func fmtSec(v float64) string {
	if v < 0 {
		return "  —  "
	}
	return fmt.Sprintf("%.2fs", v)
}

// benchPercentile expects a sorted slice.
func benchPercentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return -1
	}
	idx := int(p/100*float64(len(sorted))+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func (b *benchModel) View() string {
	dimSt := lipgloss.NewStyle().Foreground(colorDim)
	errSt := lipgloss.NewStyle().Foreground(colorEmber)
	host := b.cfg.BaseURL
	if i := strings.Index(host, "//"); i >= 0 {
		host = host[i+2:]
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(colorGold).
		Render("kaal bench") +
		dimSt.Render(" · "+host+" · "+b.cfg.Model)

	if !b.done {
		n := len(b.results)
		barW := 22
		filled := barW * n / maxInt(b.cfg.Requests, 1)
		bar := lipgloss.NewStyle().Foreground(colorSaffron).
			Render(strings.Repeat("▰", filled)) +
			dimSt.Render(strings.Repeat("▱", barW-filled))
		state := "running"
		if b.inflight {
			state = chakraFrames[n%len(chakraFrames)] + " opening…"
		}
		head := title + "\n\n" + bar + fmt.Sprintf(" %d/%d ", n, b.cfg.Requests) + state + "\n\n"

		show := n
		maxRows := b.width/4 + 4
		if maxRows > 12 {
			maxRows = 12
		}
		if show > maxRows {
			show = maxRows
		}
		var rows []string
		for i := n - show; i < n; i++ {
			r := b.results[i]
			line := fmt.Sprintf("#%-3d ttft %s total %s", r.index+1, fmtSec(r.ttft), fmtSec(r.total))
			if r.err != "" {
				rows = append(rows, errSt.Render(fmt.Sprintf("#%-3d FAILED %s",
					r.index+1, truncateRunes(r.err, 60))))
			} else {
				rows = append(rows, dimSt.Render(line+"  ✓"))
			}
		}
		foot := dimSt.Render("ctrl+c abort")
		return lipgloss.JoinVertical(lipgloss.Left, head, strings.Join(rows, "\n"), "", foot)
	}

	// Report card.
	ok, failed := 0, 0
	var ttfts, totals []float64
	for _, r := range b.results {
		if r.err != "" {
			failed++
			continue
		}
		ok++
		if r.ttft < 0 {
			r.ttft = r.total
		}
		ttfts = append(ttfts, r.ttft)
		totals = append(totals, r.total)
	}
	sort.Float64s(ttfts)
	sort.Float64s(totals)

	nameSt := lipgloss.NewStyle().Foreground(colorSaffron).Bold(true)
	valSt := lipgloss.NewStyle().Foreground(colorIvory)
	metricRow := func(label string, vals []float64) string {
		if len(vals) == 0 {
			return dimSt.Render(fmt.Sprintf("%-6s (no successful requests)", label))
		}
		cells := ""
		for _, spec := range []struct {
			name string
			p    float64
		}{{"p50", 50}, {"p75", 75}, {"p90", 90}, {"p95", 95}, {"p99", 99}} {
			cells += dimSt.Render(spec.name+" ") + valSt.Render(fmtSec(benchPercentile(vals, spec.p))) + " "
		}
		mean := 0.0
		for _, v := range vals {
			mean += v
		}
		mean /= float64(len(vals))
		return nameSt.Render(fmt.Sprintf("%-6s", label)) + cells +
			dimSt.Render("μ ") + valSt.Render(fmtSec(mean))
	}

	body := strings.Join([]string{
		dimSt.Render(fmt.Sprintf("%s · %d requests · %d ok · %d failed",
			host, b.cfg.Requests, ok, failed)),
		"",
		metricRow("TTFT", ttfts),
		metricRow("TOTAL", totals),
		"",
		dimSt.Render("q/esc close"),
	}, "\n")
	card := lipgloss.NewStyle().Width(76).
		Border(lipgloss.RoundedBorder()).BorderForeground(colorSaffron).
		Padding(0, 1).Render(title + "\n\n" + body)
	return lipgloss.NewStyle().Width(b.width).Align(lipgloss.Center).
		Render(strings.Repeat("\n", modalTopMargin) + card)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// RunBench runs the interactive benchmark program; returns the process exit
// code (0 ok, 130 aborted, 1 all-failed/program error).
func RunBench(cfg BenchConfig) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx // cancellation flows through the model's cancel on ctrl+c
	m := newBenchModel(cfg, cancel)
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.send = p.Send
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "kaal: bench:", err)
		return 1
	}
	if m.aborted {
		return 130
	}
	ok := 0
	for _, r := range m.results {
		if r.err == "" {
			ok++
		}
	}
	if ok == 0 && cfg.Requests > 0 {
		return 1
	}
	return 0
}

// BenchJSON is the machine-readable mode: sequential requests, then one JSON
// line with raw samples (same schema as scripts/benchmark.py --json).
func BenchJSON(cfg BenchConfig, w io.Writer) int {
	out := struct {
		TTFT   []float64 `json:"ttft"`
		Total  []float64 `json:"total"`
		Failed int       `json:"failed"`
	}{}
	for i := 0; i < cfg.Requests; i++ {
		r := benchRequest(context.Background(), cfg, i)
		if r.err != "" {
			out.Failed++
			continue
		}
		ttft := r.ttft
		if ttft < 0 {
			ttft = r.total
		}
		out.TTFT = append(out.TTFT, ttft)
		out.Total = append(out.Total, r.total)
	}
	enc := json.NewEncoder(w)
	_ = enc.Encode(out)
	if out.Failed > 0 && len(out.TTFT) == 0 && cfg.Requests > 0 {
		return 1
	}
	return 0
}
