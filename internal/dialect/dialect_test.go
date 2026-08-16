// Ported verbatim from tests/test_dialect.py (299 lines) — the ground truth
// for DSML healing. The fixtures and assertions below mirror the Python
// tables exactly; if a behavior drifts, this file is the referee.
package dialect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/dialect"
	"github.com/kaal/kaal/internal/messages"
)

// Unicode markers, built from escapes to avoid encoding accidents.
const (
	fw = "\uff5c" // fullwidth pipe ｜
	b  = "\u2581" // block glyph ▁
)

var (
	sOpen  = "<" + fw + "DSML" + fw + "tool_calls>"
	sClose = "</" + fw + "DSML" + fw + "tool_calls>"
	iOpen  = "<" + fw + "DSML" + fw + "invoke"
	iClose = "</" + fw + "DSML" + fw + "invoke>"
	pOpen  = "<" + fw + "DSML" + fw + "parameter"
	pClose = "</" + fw + "DSML" + fw + "parameter>"

	fullwidthSingle = sOpen +
		iOpen + ` name="get_weather">` +
		pOpen + ` name="location" string="true">San Francisco, CA` + pClose +
		iClose +
		sClose

	asciiSingle = "<|DSML|tool_calls>" +
		`<|DSML|invoke name="add">` +
		`<|DSML|parameter name="a" string="false">15</|DSML|parameter>` +
		"</|DSML|invoke>" +
		"</|DSML|tool_calls>"

	objParam = sOpen +
		iOpen + ` name="log">` +
		pOpen + ` name="payload" string="false">{"a":1}` + pClose +
		iClose +
		sClose

	chained = sOpen + "\n" +
		"\n  " + iOpen + ` name="first">` + "\n" +
		pOpen + ` name="x" string="true">1` + pClose + "\n" +
		"  " + iClose + "\n" +
		iOpen + ` name="second">` +
		pOpen + ` name="y" string="false">2` + pClose +
		iClose + "\n" +
		sClose

	leakInProse = "The answer is 42" + sOpen +
		iOpen + ` name="get_weather">` +
		pOpen + ` name="location" string="true">San Francisco, CA` + pClose +
		iClose +
		sClose + " done"

	leakedTokens = "<" + fw + "begin" + b + "of" + b + "sentence" + fw + ">hello<" + fw + "Assistant" + fw + ">"

	thinkSpan = "<think>let me check</think>The weather is 18°C"

	unclosedSection = sOpen + iOpen + ` name="x">`

	// The model QUOTES the envelope in prose (ASCII form, backticks) and
	// keeps writing — no close tag anywhere in the rest of the answer.
	proseQuote = "The first sentence establishes context. " +
		"Now, quoting the wire format: `<|DSML|tool_calls>` " +
		"is what the model emits when it wants to call a tool. " +
		"This remaining part of the answer must be preserved " +
		"entirely, including this final clause."

	// A FULL envelope (open + invoke + parameter + close) quoted inside prose.
	completeProseEnvelope = "Here is the complete envelope the model produces: " +
		"<|DSML|tool_calls>" +
		`<|DSML|invoke name="read">` +
		`<|DSML|parameter name="path" string="true">x</|DSML|parameter>` +
		"</|DSML|invoke>" +
		"</|DSML|tool_calls>" +
		" — that is all, and the trailing prose survives."
)

const truncatedSuffix = "\u2026[parameter truncated]" // fullwidth ellipsis …

func feedCharwise(text string) []dialect.Event {
	feed := dialect.NewDialectFeed()
	var events []dialect.Event
	for _, r := range text {
		events = append(events, feed.Feed(string(r))...)
	}
	events = append(events, feed.Flush()...)
	return events
}

// canonicalize collapses consecutive text/reasoning runs; keeps tool calls
// intact (mirrors the Python helper).
func canonicalize(events []dialect.Event) []dialect.Event {
	out := []dialect.Event{}
	for _, ev := range events {
		if ev.Kind == dialect.EventToolCall {
			out = append(out, ev)
		} else if len(out) > 0 && out[len(out)-1].Kind == ev.Kind {
			last := &out[len(out)-1]
			last.Text += ev.Text
		} else {
			out = append(out, ev)
		}
	}
	return out
}

func joinedText(events []dialect.Event) string {
	var sb []string
	for _, ev := range events {
		if ev.Kind == dialect.EventText {
			sb = append(sb, ev.Text)
		}
	}
	return concat(sb)
}

func concat(parts []string) string {
	out := ""
	for _, p := range parts {
		out += p
	}
	return out
}

func TestFullwidthSingleCall(t *testing.T) {
	events := dialect.NewDialectFeed().Feed(fullwidthSingle)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	want := messages.ToolCall{ID: "call_1", Name: "get_weather", Arguments: `{"location": "San Francisco, CA"}`}
	if events[0].Kind != dialect.EventToolCall || events[0].Call != want {
		t.Fatalf("want tool_call %+v, got %+v", want, events[0])
	}
	for _, ev := range events {
		if ev.Kind == dialect.EventText {
			t.Fatalf("unexpected text event: %q", ev.Text)
		}
	}
}

func TestASCIIVariant(t *testing.T) {
	events := dialect.NewDialectFeed().Feed(asciiSingle)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	want := messages.ToolCall{ID: "call_1", Name: "add", Arguments: `{"a": 15}`}
	if events[0].Call != want {
		t.Fatalf("want %+v, got %+v", want, events[0].Call)
	}
}

func TestFalseStringObject(t *testing.T) {
	events := dialect.NewDialectFeed().Feed(objParam)
	want := messages.ToolCall{ID: "call_1", Name: "log", Arguments: `{"payload": {"a": 1}}`}
	if events[0].Call != want {
		t.Fatalf("want %+v, got %+v", want, events[0].Call)
	}
}

func TestChainedInvokesOrder(t *testing.T) {
	events := dialect.NewDialectFeed().Feed(chained)
	var calls []messages.ToolCall
	for _, ev := range events {
		if ev.Kind == dialect.EventToolCall {
			calls = append(calls, ev.Call)
		}
	}
	want := []messages.ToolCall{
		{ID: "call_1", Name: "first", Arguments: `{"x": "1"}`},
		{ID: "call_2", Name: "second", Arguments: `{"y": 2}`},
	}
	if len(calls) != len(want) {
		t.Fatalf("want %d calls, got %d: %+v", len(want), len(calls), calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d: want %+v, got %+v", i, want[i], calls[i])
		}
	}
}

func TestCallCounterPersistsAcrossSections(t *testing.T) {
	feed := dialect.NewDialectFeed()
	first := feed.Feed(fullwidthSingle)
	second := feed.Feed(fullwidthSingle)
	if first[0].Call.ID != "call_1" {
		t.Fatalf("first call id: want call_1, got %s", first[0].Call.ID)
	}
	if second[0].Call.ID != "call_2" {
		t.Fatalf("second call id: want call_2, got %s", second[0].Call.ID)
	}
}

func TestLeakInProse(t *testing.T) {
	events := dialect.NewDialectFeed().Feed(leakInProse)
	for _, ev := range events {
		if ev.Kind == dialect.EventToolCall {
			t.Fatalf("prose quote must not heal a tool call: %+v", ev)
		}
	}
	want := "The answer is 42" +
		iOpen + ` name="get_weather">` +
		pOpen + ` name="location" string="true">San Francisco, CA` +
		"done"
	if got := joinedText(events); got != want {
		t.Fatalf("joined text:\nwant %q\n got %q", want, got)
	}
}

func TestLeakedChatTokens(t *testing.T) {
	events := dialect.NewDialectFeed().Feed(leakedTokens)
	if len(events) != 1 || events[0].Kind != dialect.EventText || events[0].Text != "hello" {
		t.Fatalf("want [text hello], got %+v", events)
	}
}

func TestThinkSpan(t *testing.T) {
	events := dialect.NewDialectFeed().Feed(thinkSpan)
	var reasoning, text []string
	for _, ev := range events {
		switch ev.Kind {
		case dialect.EventReasoning:
			reasoning = append(reasoning, ev.Text)
		case dialect.EventText:
			text = append(text, ev.Text)
		}
	}
	if got := concat(reasoning); got != "let me check" {
		t.Fatalf("reasoning: want %q, got %q", "let me check", got)
	}
	if got := concat(text); got != "The weather is 18°C" {
		t.Fatalf("text: want %q, got %q", "The weather is 18°C", got)
	}
}

func TestUnclosedSectionFlush(t *testing.T) {
	feed := dialect.NewDialectFeed()
	if events := feed.Feed(unclosedSection); len(events) != 0 {
		t.Fatalf("want no events, got %+v", events)
	}
	// Generation-leading open with no complete invoke: recovered (nothing
	// buffered, so no text event) rather than discarded.
	if events := feed.Flush(); len(events) != 0 {
		t.Fatalf("want no flush events, got %+v", events)
	}
}

func TestProseQuoteMidAnswerPreservesRest(t *testing.T) {
	feed := dialect.NewDialectFeed()
	events := append(feed.Feed(proseQuote), feed.Flush()...)
	for _, ev := range events {
		if ev.Kind == dialect.EventToolCall {
			t.Fatalf("prose quote must not heal a tool call: %+v", ev)
		}
	}
	text := joinedText(events)
	for _, want := range []string{
		"is what the model emits when it wants to call a tool.",
		"This remaining part of the answer must be preserved",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if !strings.HasSuffix(text, "including this final clause.") {
		t.Fatalf("want suffix %q in %q", "including this final clause.", text)
	}
	if strings.Contains(text, "DSML") {
		t.Fatalf("DSML marker leaked into text: %q", text)
	}
	if strings.Contains(text, "<|DSML|tool_calls>") {
		t.Fatalf("envelope open leaked into text: %q", text)
	}
}

func TestCompleteProseEnvelopeNoPhantomCall(t *testing.T) {
	feed := dialect.NewDialectFeed()
	events := append(feed.Feed(completeProseEnvelope), feed.Flush()...)
	for _, ev := range events {
		if ev.Kind == dialect.EventToolCall {
			t.Fatalf("quoted envelope must not heal a phantom tool call: %+v", ev)
		}
	}
	text := joinedText(events)
	if !strings.Contains(text, "Here is the complete envelope the model produces: ") {
		t.Fatalf("missing leading prose in %q", text)
	}
	// The inner invoke/parameter opens and the parameter value pass through
	// as the text the model wrote…
	if !strings.Contains(text, `<|DSML|invoke name="read">`) {
		t.Fatalf("missing invoke open in %q", text)
	}
	if !strings.Contains(text, `name="path" string="true">x`) {
		t.Fatalf("missing parameter open+value in %q", text)
	}
	// …and the trailing prose survives (leading space swallowed with the
	// section close's control-token handling).
	if !strings.HasSuffix(text, "— that is all, and the trailing prose survives.") {
		t.Fatalf("want trailing prose suffix in %q", text)
	}
}

func TestGenerationLeadingEnvelopeStillHeals(t *testing.T) {
	feed := dialect.NewDialectFeed()
	first := feed.Feed(fullwidthSingle)
	if len(first) != 1 || first[0].Kind != dialect.EventToolCall {
		t.Fatalf("generation-leading envelope must heal: %+v", first)
	}
	want := messages.ToolCall{ID: "call_1", Name: "get_weather", Arguments: `{"location": "San Francisco, CA"}`}
	if first[0].Call != want {
		t.Fatalf("want %+v, got %+v", want, first[0].Call)
	}
	// A LATER chunk in the same feed may quote the envelope in prose: the
	// quote must not produce a second tool call, and the prose survives.
	quote := "To be clear, the envelope looks like `<|DSML|tool_calls>` and that's it."
	later := append(feed.Feed(quote), feed.Flush()...)
	for _, ev := range later {
		if ev.Kind == dialect.EventToolCall {
			t.Fatalf("later quote must not heal a tool call: %+v", ev)
		}
	}
	if got := joinedText(later); got != "To be clear, the envelope looks like `` and that's it." {
		t.Fatalf("want %q, got %q", "To be clear, the envelope looks like `` and that's it.", got)
	}
}

func TestThinkSpanThenEnvelopeStillHeals(t *testing.T) {
	// DeepSeek's chat template emits the envelope right after the think
	// span — reasoning must NOT count as emitted text, or a real
	// generation-leading envelope after </think> would be mistaken for a
	// prose quote.
	feed := dialect.NewDialectFeed()
	events := append(feed.Feed("<think>let me check</think>"), feed.Feed(fullwidthSingle)...)
	found := false
	for _, ev := range events {
		if ev.Kind == dialect.EventToolCall {
			found = true
			want := messages.ToolCall{ID: "call_1", Name: "get_weather", Arguments: `{"location": "San Francisco, CA"}`}
			if ev.Call != want {
				t.Fatalf("want %+v, got %+v", want, ev.Call)
			}
		}
	}
	if !found {
		t.Fatalf("envelope after think span must heal: %+v", events)
	}
}

func TestBoundarySafety(t *testing.T) {
	for _, fixture := range []string{
		fullwidthSingle,
		asciiSingle,
		chained,
		leakInProse,
		proseQuote,
		completeProseEnvelope,
		thinkSpan,
	} {
		whole := dialect.NewDialectFeed()
		wholeEvents := append(whole.Feed(fixture), whole.Flush()...)
		got := canonicalize(wholeEvents)
		want := canonicalize(feedCharwise(fixture))
		if len(got) != len(want) {
			t.Fatalf("fixture %q: charwise %d events vs whole %d", fixture[:min(40, len(fixture))], len(want), len(got))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("fixture %q event %d:\n charwise %+v\n whole    %+v", fixture[:min(40, len(fixture))], i, want[i], got[i])
			}
		}
	}
}

func TestParameterCap(t *testing.T) {
	value := strings.Repeat("a", 1_000_001)
	feed := dialect.NewDialectFeed()
	var events []dialect.Event
	prefix := sOpen + iOpen + ` name="big">` + pOpen + ` name="data" string="true">`
	events = append(events, feed.Feed(prefix)...)
	const chunk = 100_000
	for start := 0; start < len(value); start += chunk {
		end := min(start+chunk, len(value))
		events = append(events, feed.Feed(value[start:end])...)
	}
	events = append(events, feed.Feed(pClose+iClose+sClose)...)
	var calls []messages.ToolCall
	for _, ev := range events {
		if ev.Kind == dialect.EventToolCall {
			calls = append(calls, ev.Call)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	var args struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("bad arguments JSON: %v", err)
	}
	kept := args.Data
	if !strings.HasSuffix(kept, truncatedSuffix) {
		t.Fatalf("want truncated suffix, got tail %q", kept[max(0, len(kept)-len(truncatedSuffix)):])
	}
	wantLen := 1_000_000 + len(truncatedSuffix)
	if len(kept) != wantLen {
		t.Fatalf("want length %d, got %d", wantLen, len(kept))
	}
	if !strings.HasPrefix(kept, strings.Repeat("a", 1_000_000)) {
		t.Fatalf("want a-prefix, got head %q", kept[:16])
	}
}

func TestStrayLTPassthrough(t *testing.T) {
	events := dialect.NewDialectFeed().Feed("a < b")
	if len(events) != 1 || events[0].Kind != dialect.EventText || events[0].Text != "a < b" {
		t.Fatalf("want [text \"a < b\"], got %+v", events)
	}
}

// -- tiny helpers -------------------------------------------------------------

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
