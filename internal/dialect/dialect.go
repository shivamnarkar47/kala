// Package dialect ports harness/dialect.py — the incremental DSML envelope
// parser/healer plus leaked-token stripper.
//
// DeepSeek V4 emits tool calls inside a DSML (XML-style) envelope that, on
// this gateway, leaks into the model's visible content deltas instead of
// arriving as structured tool_calls. DialectFeed is an incremental state
// machine that heals those envelopes back into messages.ToolCall values as
// chunks arrive, and strips leaked chat-template tokens out of the visible
// text.
//
// All envelope markers use their exact Unicode forms (fullwidth pipe U+FF5C
// and the block glyph U+2581); the model never trained on ASCII substitutes,
// so the markers are load-bearing and must never be transliterated.
package dialect

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/messages"
)

// Unicode markers, built from escapes to avoid encoding accidents.
const (
	FW = "\uff5c" // fullwidth pipe ｜
	B  = "\u2581" // block glyph ▁
)

// DSML envelope tags. The open tags come in (fullwidth, ascii) variants; the
// invoke/parameter *open* entries are prefixes — the full tag carries a
// name="..." / string="..." attribute before its closing ">".
var (
	sectionOpen  = []string{"<" + FW + "DSML" + FW + "tool_calls>", "<|DSML|tool_calls>"}
	sectionClose = []string{"</" + FW + "DSML" + FW + "tool_calls>", "</|DSML|tool_calls>"}
	invokeOpen   = []string{"<" + FW + "DSML" + FW + "invoke", "<|DSML|invoke"}
	invokeClose  = []string{"</" + FW + "DSML" + FW + "invoke>", "</|DSML|invoke>"}
	paramOpen    = []string{"<" + FW + "DSML" + FW + "parameter", "<|DSML|parameter"}
	paramClose   = []string{"</" + FW + "DSML" + FW + "parameter>", "</|DSML|parameter>"}

	thinkOpen  = "<think>"
	thinkClose = "</think>"

	// Chat-template control tokens to strip from visible text (all
	// fullwidth-pipe forms except the ASCII "<|EOT|>").
	controlTokens = []string{
		"<" + FW + "begin" + B + "of" + B + "sentence" + FW + ">",
		"<" + FW + "end" + B + "of" + B + "sentence" + FW + ">",
		"<" + FW + B + "pad" + FW + ">",
		"<" + FW + "User" + FW + ">",
		"<" + FW + "Assistant" + FW + ">",
		"<|EOT|>",
		"<" + FW + "search" + B + "begin" + FW + ">",
		"<" + FW + "search" + B + "end" + FW + ">",
		"<" + FW + "fim" + B + "hole" + FW + ">",
		"<" + FW + "end" + B + "of" + B + "turn" + FW + ">",
	}
)

const (
	maxParameterChars  = 1_000_000
	truncatedSuffix    = "\u2026[parameter truncated]" // fullwidth ellipsis …
	paramOverflowSlack = 1024
)

// Parser states.
type state int

const (
	stOutside state = iota
	stThink
	stSection
	stInvoke
	stParam
)

// EventKind classifies one parsed event: text | reasoning | tool_call.
type EventKind int

const (
	EventText EventKind = iota
	EventReasoning
	EventToolCall
)

// Event is one parsed event from the feed.
type Event struct {
	Kind EventKind
	Text string
	Call messages.ToolCall
}

// tokenInfo is a precomputed token: the string, its rune count, and the byte
// length of each proper rune prefix (for rune-exact suffix/prefix math on
// UTF-8 strings).
type tokenInfo struct {
	text        string
	runes       int
	prefixBytes []prefixEntry
}

type prefixEntry struct {
	k     int // runes
	bytes int // byte length of the first k runes
}

func buildToken(text string) tokenInfo {
	info := tokenInfo{text: text, runes: utf8.RuneCountInString(text)}
	runeIdx := 0
	byteLen := 0
	for _, r := range text {
		runeIdx++
		byteLen += utf8.RuneLen(r)
		if runeIdx < info.runes {
			info.prefixBytes = append(info.prefixBytes, prefixEntry{k: runeIdx, bytes: byteLen})
		}
	}
	return info
}

// Token sets per parser state, matching the Python tuples.
var (
	outsideTokens  = buildTokens(sectionOpen, sectionClose, invokeClose, paramClose, controlTokens, []string{thinkOpen, thinkClose})
	thinkTokens    = buildTokens(controlTokens, []string{thinkClose})
	sectionTags    = buildTokens(sectionClose, invokeOpen, invokeClose, paramOpen, paramClose)
	invokeTags     = buildTokens(invokeClose, paramOpen, paramClose)
	paramCloseInfo = buildTokens(paramClose)

	// Raw tag lists as tokenInfos for the step functions.
	sectionCloseInfo = buildTokens(sectionClose)
	invokeOpenInfo   = buildTokens(invokeOpen)
	invokeCloseInfo  = buildTokens(invokeClose)
	paramOpenInfo    = buildTokens(paramOpen)

	// Every token in every parser state starts with "<", so any partial-token
	// suffix of a buffer must contain "<" within its last maxTokenPrefix
	// runes (the longest proper token prefix). Chunks whose tail has no "<"
	// cannot be mid-token, so skip the O(tokens x prefix) suffix scan.
	maxTokenPrefix = 0
)

func buildTokens(groups ...[]string) []tokenInfo {
	var out []tokenInfo
	for _, g := range groups {
		for _, s := range g {
			out = append(out, buildToken(s))
		}
	}
	return out
}

func init() {
	for _, t := range append(append(append([]tokenInfo{}, outsideTokens...), thinkTokens...), paramCloseInfo...) {
		if t.runes-1 > maxTokenPrefix {
			maxTokenPrefix = t.runes - 1
		}
	}
}

// DialectFeed is the incremental healer for leaked DSML envelopes and
// chat-template tokens.
//
// Feed content chunks to Feed and collect the events it returns; call Flush
// at end of stream to drain remaining buffered text (or to discard an
// unclosed DSML section that parsed at least one invoke — an unclosed
// section with no invokes is recovered as visible text).
type DialectFeed struct {
	buffer         string
	state          state
	swallowWS      bool // drop leading whitespace after a stripped token
	callCounter    int  // single counter; never resets across sections
	calls          []messages.ToolCall
	textEmitted    bool // any visible text emitted in this feed's lifetime
	sectionInvokes int  // complete invokes parsed in the current section
	invokeName     string
	args           *jsonpy.OrderedMap
	paramName      string
	paramIsString  bool
	paramValue     []string
	paramLen       int
	paramOverflow  bool
}

func NewDialectFeed() *DialectFeed {
	return &DialectFeed{state: stOutside}
}

// Feed processes one chunk and returns events newly parsed from it (empty
// input yields no events).
func (f *DialectFeed) Feed(text string) []Event {
	if text == "" {
		return nil
	}
	f.buffer += text
	return f.process()
}

// Flush ends the stream: drain remaining text; an unclosed DSML section that
// parsed at least one invoke is discarded, while a section that never
// produced a call is recovered as visible text — it was almost certainly a
// prose quote of the envelope, not a real tool call.
func (f *DialectFeed) Flush() []Event {
	var events []Event
	if f.swallowWS {
		f.buffer = trimLeftWS(f.buffer)
		f.swallowWS = false
	}
	switch f.state {
	case stSection, stInvoke, stParam:
		if f.sectionInvokes == 0 {
			// False positive: no complete invoke was ever parsed, so this
			// open was a prose quote of the envelope (or a real open
			// truncated before its first call). Recover the buffered bytes
			// as visible text instead of discarding the answer.
			if f.buffer != "" {
				events = append(events, Event{Kind: EventText, Text: stripControls(f.buffer)})
				f.buffer = ""
			}
			f.resetSection()
			return events
		}
		// Discarding unclosed DSML section at end of stream.
		f.resetSection()
		return nil
	}
	if f.buffer != "" {
		if f.state == stThink {
			events = append(events, Event{Kind: EventReasoning, Text: stripControls(f.buffer)})
		} else {
			events = append(events, Event{Kind: EventText, Text: stripControls(f.buffer)})
		}
		f.buffer = ""
	}
	f.state = stOutside
	return events
}

func (f *DialectFeed) resetSection() {
	f.calls = nil
	f.sectionInvokes = 0
	f.invokeName = ""
	f.args = nil
	f.paramValue = nil
	f.paramLen = 0
	f.paramOverflow = false
	f.state = stOutside
}

func (f *DialectFeed) process() []Event {
	var events []Event
	for f.buffer != "" {
		var stepEvents []Event
		var progressed bool
		switch f.state {
		case stOutside:
			stepEvents, progressed = f.stepOutside()
		case stThink:
			stepEvents, progressed = f.stepThink()
		case stSection:
			stepEvents, progressed = f.stepSection()
		case stInvoke:
			stepEvents, progressed = f.stepInvoke()
		default:
			stepEvents, progressed = f.stepParam()
		}
		events = append(events, stepEvents...)
		if !progressed {
			break // holding a partial token prefix; wait for more input
		}
	}
	return events
}

func (f *DialectFeed) stepOutside() ([]Event, bool) {
	var events []Event
	if f.swallowWS {
		f.buffer = trimLeftWS(f.buffer)
		f.swallowWS = false
		if f.buffer == "" {
			return events, true
		}
	}
	index, tok := findEarliestToken(f.buffer, outsideTokens)
	if tok == nil {
		k, bytes := partialSuffixOverlap(f.buffer, outsideTokens)
		if k > 0 {
			if bytes < len(f.buffer) {
				events = append(events, Event{Kind: EventText, Text: stripControls(f.buffer[:len(f.buffer)-bytes])})
				f.textEmitted = true
				f.buffer = f.buffer[len(f.buffer)-bytes:]
				return events, true
			}
			return events, false // whole buffer is a partial token prefix
		}
		events = append(events, Event{Kind: EventText, Text: stripControls(f.buffer)})
		f.textEmitted = true
		f.buffer = ""
		return events, true
	}
	if index > 0 {
		events = append(events, Event{Kind: EventText, Text: stripControls(f.buffer[:index])})
		f.textEmitted = true
	}
	f.buffer = f.buffer[index:]
	if isSectionOpen(tok) {
		if f.textEmitted {
			// The model QUOTED the envelope in prose (e.g. explaining the
			// wire format): real envelopes are generation-leading, so this
			// open cannot be a real section. Strip it like a control token
			// and stay outside — the invoke/parameter tags that follow are
			// not outside-state tokens, so they pass through as the visible
			// text the model actually wrote.
			f.buffer = f.buffer[len(tok.text):]
			f.swallowWS = true
			return events, true
		}
		f.buffer = f.buffer[len(tok.text):]
		f.calls = nil
		f.sectionInvokes = 0
		f.state = stSection
		return events, true
	}
	if tok.text == thinkOpen {
		f.buffer = f.buffer[len(tok.text):]
		f.state = stThink
		f.swallowWS = true
		return events, true
	}
	// Control token or stray close token with no open: drop it together
	// with any immediately following template padding.
	f.buffer = f.buffer[len(tok.text):]
	f.swallowWS = true
	return events, true
}

func (f *DialectFeed) stepThink() ([]Event, bool) {
	var events []Event
	if f.swallowWS {
		f.buffer = trimLeftWS(f.buffer)
		f.swallowWS = false
		if f.buffer == "" {
			return events, true
		}
	}
	index, tok := findEarliestToken(f.buffer, thinkTokens)
	if tok == nil {
		k, bytes := partialSuffixOverlap(f.buffer, thinkTokens)
		if k > 0 {
			if bytes < len(f.buffer) {
				events = append(events, Event{Kind: EventReasoning, Text: stripControls(f.buffer[:len(f.buffer)-bytes])})
				f.buffer = f.buffer[len(f.buffer)-bytes:]
				return events, true
			}
			return events, false
		}
		events = append(events, Event{Kind: EventReasoning, Text: stripControls(f.buffer)})
		f.buffer = ""
		return events, true
	}
	if index > 0 {
		events = append(events, Event{Kind: EventReasoning, Text: stripControls(f.buffer[:index])})
	}
	f.buffer = f.buffer[index:]
	if tok.text == thinkClose {
		f.buffer = f.buffer[len(tok.text):]
		f.state = stOutside
		f.swallowWS = true
		return events, true
	}
	// Control token inside the think span: strip it and its padding.
	f.buffer = f.buffer[len(tok.text):]
	f.swallowWS = true
	return events, true
}

func (f *DialectFeed) stepSection() ([]Event, bool) {
	var events []Event
	f.buffer = trimLeftWS(f.buffer) // whitespace between tags is insignificant
	if f.buffer == "" {
		return events, true
	}
	if tok := startsWithAny(f.buffer, sectionCloseInfo); tok != nil {
		f.buffer = f.buffer[len(tok.text):]
		calls := f.calls
		f.calls = nil
		for _, c := range calls {
			events = append(events, Event{Kind: EventToolCall, Call: c})
		}
		f.state = stOutside
		return events, true
	}
	if tok := startsWithAny(f.buffer, invokeOpenInfo); tok != nil {
		end := strings.IndexByte(f.buffer, '>')
		if end == -1 {
			return events, false // incomplete open tag: wait for the ">"
		}
		tag := f.buffer[:end+1]
		f.buffer = f.buffer[end+1:]
		f.invokeName = attrValue(tag, "name")
		f.args = jsonpy.NewOrderedMap()
		f.state = stInvoke
		return events, true
	}
	if isProperPrefixOfAny(f.buffer, sectionTags) {
		return events, false // partial tag: wait for more input
	}
	// Malformed content inside the envelope: consume up to the next "<".
	nextLT := strings.IndexByte(f.buffer, '<')
	switch {
	case nextLT == -1:
		f.buffer = ""
	case nextLT == 0:
		f.buffer = f.buffer[1:]
	default:
		f.buffer = f.buffer[nextLT:]
	}
	return events, true
}

func (f *DialectFeed) stepInvoke() ([]Event, bool) {
	var events []Event
	f.buffer = trimLeftWS(f.buffer)
	if f.buffer == "" {
		return events, true
	}
	if tok := startsWithAny(f.buffer, invokeCloseInfo); tok != nil {
		f.buffer = f.buffer[len(tok.text):]
		f.callCounter++
		argsJSON, err := jsonpy.Marshal(f.args)
		if err != nil {
			argsJSON = []byte("{}") // unreachable for our value types; keep the call alive
		}
		f.calls = append(f.calls, messages.ToolCall{
			ID:        fmt.Sprintf("call_%d", f.callCounter),
			Name:      f.invokeName,
			Arguments: string(argsJSON),
		})
		f.sectionInvokes++
		f.state = stSection
		return events, true
	}
	if tok := startsWithAny(f.buffer, paramOpenInfo); tok != nil {
		end := strings.IndexByte(f.buffer, '>')
		if end == -1 {
			return events, false // incomplete open tag: wait for the ">"
		}
		tag := f.buffer[:end+1]
		f.buffer = f.buffer[end+1:]
		f.paramName = attrValue(tag, "name")
		f.paramIsString = attrValue(tag, "string") != "false"
		f.paramValue = nil
		f.paramLen = 0
		f.paramOverflow = false
		f.state = stParam
		return events, true
	}
	if isProperPrefixOfAny(f.buffer, invokeTags) {
		return events, false
	}
	nextLT := strings.IndexByte(f.buffer, '<')
	switch {
	case nextLT == -1:
		f.buffer = ""
	case nextLT == 0:
		f.buffer = f.buffer[1:]
	default:
		f.buffer = f.buffer[nextLT:]
	}
	return events, true
}

func (f *DialectFeed) stepParam() ([]Event, bool) {
	var events []Event
	index, tok := findEarliestToken(f.buffer, paramCloseInfo)
	if tok == nil {
		k, bytes := partialSuffixOverlap(f.buffer, paramCloseInfo)
		if k > 0 {
			keep := f.buffer[:len(f.buffer)-bytes]
			f.buffer = f.buffer[len(f.buffer)-bytes:]
			if keep != "" {
				f.appendParamValue(keep)
			}
			return events, keep != ""
		}
		f.appendParamValue(f.buffer)
		f.buffer = ""
		return events, true
	}
	f.appendParamValue(f.buffer[:index])
	f.buffer = f.buffer[index+len(tok.text):]
	raw := strings.Join(f.paramValue, "")
	if utf8.RuneCountInString(raw) > maxParameterChars {
		raw = truncateToRunes(raw, maxParameterChars) + truncatedSuffix
	}
	f.args.Set(f.paramName, coerceValue(raw, f.paramIsString))
	f.state = stInvoke
	return events, true
}

func (f *DialectFeed) appendParamValue(part string) {
	if f.paramOverflow {
		return
	}
	f.paramValue = append(f.paramValue, part)
	f.paramLen += utf8.RuneCountInString(part)
	if f.paramLen > maxParameterChars+paramOverflowSlack {
		f.paramOverflow = true
	}
}

// -- pure helpers ------------------------------------------------------------

// findEarliestToken returns the earliest occurrence of any token in text;
// longest token wins ties (prefix safety: a longer token at the same index
// must not be shadowed by a shorter one). Returns (-1, nil) when absent.
func findEarliestToken(text string, tokens []tokenInfo) (int, *tokenInfo) {
	bestIndex := -1
	var bestTok *tokenInfo
	for i := range tokens {
		t := &tokens[i]
		idx := strings.Index(text, t.text)
		if idx == -1 {
			continue
		}
		if bestIndex == -1 || idx < bestIndex || (idx == bestIndex && t.runes > bestTok.runes) {
			bestIndex = idx
			bestTok = t
		}
	}
	if bestTok == nil {
		return -1, nil
	}
	return bestIndex, bestTok
}

// partialSuffixOverlap returns the longest suffix of text that is a proper
// prefix of any token, as (runes, bytes) — runes to mirror Python's len(),
// bytes to slice the buffer exactly.
func partialSuffixOverlap(text string, tokens []tokenInfo) (int, int) {
	if text == "" {
		return 0, 0
	}
	if !strings.Contains(lastNRunes(text, maxTokenPrefix), "<") {
		return 0, 0
	}
	textRunes := utf8.RuneCountInString(text)
	bestK, bestBytes := 0, 0
	for i := range tokens {
		t := &tokens[i]
		limit := min(textRunes, t.runes-1)
		for j := limit; j >= 1; j-- {
			e := t.prefixBytes[j-1]
			if strings.HasSuffix(text, t.text[:e.bytes]) {
				if j > bestK {
					bestK, bestBytes = j, e.bytes
				}
				break
			}
		}
	}
	return bestK, bestBytes
}

// startsWithAny returns the longest token text starts with, else nil.
func startsWithAny(text string, tokens []tokenInfo) *tokenInfo {
	var best *tokenInfo
	for i := range tokens {
		t := &tokens[i]
		if len(t.text) > len(text) {
			continue
		}
		if strings.HasPrefix(text, t.text) && (best == nil || t.runes > best.runes) {
			best = t
		}
	}
	return best
}

// isProperPrefixOfAny reports whether text is a non-empty proper prefix of
// any token.
func isProperPrefixOfAny(text string, tokens []tokenInfo) bool {
	if text == "" {
		return false
	}
	textRunes := utf8.RuneCountInString(text)
	for i := range tokens {
		t := &tokens[i]
		if t.runes > textRunes && strings.HasPrefix(t.text, text) {
			return true
		}
	}
	return false
}

func isSectionOpen(tok *tokenInfo) bool {
	return tok.text == sectionOpen[0] || tok.text == sectionOpen[1]
}

// attrValue extracts `attr="..."` (preceded by whitespace, per the Python
// regex \sattr="([^"]*)") from a DSML open tag; "" when absent.
func attrValue(tag, attr string) string {
	idx := strings.Index(tag, attr+`="`)
	if idx <= 0 || !isSpaceByte(tag[idx-1]) {
		return ""
	}
	rest := tag[idx+len(attr)+2:]
	end := strings.IndexByte(rest, '"')
	if end == -1 {
		return ""
	}
	return rest[:end]
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// coerceValue mirrors json.loads on the trimmed value, falling back to the
// raw string when parsing fails. json.Number keeps ints ints ("15", not
// "15.0") and floats verbatim.
func coerceValue(raw string, isString bool) any {
	if isString {
		return raw
	}
	trimmed := strings.TrimSpace(raw)
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return raw // trailing garbage after the value
	}
	return v
}

// stripControls removes any leaked control tokens (belt-and-suspenders).
func stripControls(text string) string {
	for _, t := range controlTokens {
		text = strings.ReplaceAll(text, t, "")
	}
	return text
}

func truncateToRunes(s string, n int) string {
	i := 0
	for count := 0; count < n && i < len(s); count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// lastNRunes returns the last n runes of s (as a string).
func lastNRunes(s string, n int) string {
	i := len(s)
	for count := 0; count < n && i > 0; count++ {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

func trimLeftWS(s string) string {
	return strings.TrimLeftFunc(s, unicode.IsSpace)
}
