// CLI tests — ported from tests/test_cli.py where the behavior is testable
// without the Python mocking machinery: the run path is exercised against a
// local httptest SSE server (a real loop over a fake gateway), everything
// else against the real store/files.
package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/config"
	"github.com/kaal/kaal/internal/sessions"
)

// skipOnWindows marks tests whose fake toolchain (#!/bin/sh scripts on
// PATH) cannot execute on Windows; the real update/release paths are
// exercised on the POSIX CI legs.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake POSIX toolchain scripts cannot execute on Windows")
	}
}

// fakeSSEServer streams a fixed SSE script and records every request body.
type fakeSSEServer struct {
	server   *httptest.Server
	body     string
	status   int
	requests [][]byte
}

func newFakeSSEServer(t *testing.T, body string, status int) *fakeSSEServer {
	t.Helper()
	f := &fakeSSEServer{body: body, status: status}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.requests = append(f.requests, raw)
		if f.status != 200 {
			w.WriteHeader(f.status)
			io.WriteString(w, "bad request")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(f.body)))
		w.WriteHeader(200)
		io.WriteString(w, f.body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSSEServer) wireMessages(i int) []map[string]any {
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(f.requests[i], &body)
	return body.Messages
}

var answerSSE = "data: {\"choices\": [{\"delta\": {\"content\": \"Hello from the gateway.\\n\"}, \"finish_reason\": null}]}\n" +
	"\n" +
	"data: {\"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}]}\n" +
	"\n" +
	"data: [DONE]\n" +
	"\n"

// lengthSSE always finishes with "length" and no content: the loop's
// overflow retry fires twice, then LoopError (exit 2).
var lengthSSE = "data: {\"choices\": [{\"delta\": {}, \"finish_reason\": \"length\"}]}\n" +
	"\n" +
	"data: [DONE]\n" +
	"\n"

func runMain(t *testing.T, stdin string, argv ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(argv, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

// setupCLI points the key/sessions env at a disposable place and the run
// path at a fake gateway.
func setupCLI(t *testing.T, sse string, status int) (*fakeSSEServer, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("OPENCODE_API_KEY", "sk-test")
	t.Setenv("KAAL_SESSIONS_DIR", filepath.Join(dir, "sessions"))
	// Isolate the user config: tests must never touch the real key/model
	// stores.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	f := newFakeSSEServer(t, sse, status)
	old := runGatewayBaseURL
	runGatewayBaseURL = f.server.URL
	t.Cleanup(func() { runGatewayBaseURL = old })
	return f, dir
}

func TestVersion(t *testing.T) {
	code, out, _ := runMain(t, "", "--version")
	if code != 0 || out != "kaal 0.3\n" {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestRunSingleAnswer(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	code, out, _ := runMain(t, "", "run", "Say hello", "--dir", dir)
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out, "Hello from the gateway.") {
		t.Fatalf("out: %q", out)
	}
}

func TestRunJSONRecord(t *testing.T) {
	f, dir := setupCLI(t, answerSSE, 200)
	code, out, _ := runMain(t, "", "run", "Say hello", "--dir", dir, "--json")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &record); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if record["session_id"] == "" || record["answer"] != "Hello from the gateway.\n" {
		t.Fatalf("record: %v", record)
	}
	if record["model"] != config.ModelID { // the shipped default rides the free tier
		t.Fatalf("model: %v", record["model"])
	}
	steps, ok := record["steps"].(float64)
	if !ok || steps < 1 {
		t.Fatalf("steps: %v", record["steps"])
	}
	if tc, ok := record["tool_calls"].(float64); ok && tc != 0 {
		t.Fatalf("tool_calls: %v", record["tool_calls"])
	}
	usage, ok := record["usage"].(map[string]any)
	if !ok || usage["input_tokens"].(float64) <= 0 || usage["output_tokens"].(float64) <= 0 {
		t.Fatalf("usage: %v", record["usage"])
	}
	if _, ok := record["cost"]; !ok {
		t.Fatalf("cost missing: %v", record)
	}
	_ = f
}

func TestRunNoAPIKey(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("KAAL_SESSIONS_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("HOME", t.TempDir()) // isolate the CLI auth file + omp fallbacks
	// Pin a keyed model: the free-tier default is keyless by design.
	code, _, errOut := runMain(t, "", "run", "hi", "--dir", t.TempDir(), "--model", "deepseek-v4-flash")
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "no API key") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestRunDashReadsPromptFromStdin(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	code, _, _ := runMain(t, "hello from stdin\n", "run", "-", "--dir", dir)
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	// The task was recorded in the session store.
	store := sessions.ListSessions()
	if len(store) != 1 {
		t.Fatalf("sessions: %v", store)
	}
	if prompt, _ := store[0]["prompt"].(string); prompt != "hello from stdin" {
		t.Fatalf("prompt: %q", prompt)
	}
}

func TestRunResumeWithoutPromptDefaultsContinue(t *testing.T) {
	setupCLI(t, answerSSE, 200)
	sid := sessions.NewSessionID()
	code, _, _ := runMain(t, "", "run", "--resume", sid, "--dir", t.TempDir())
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	msgs := sessions.LoadMessages(sid)
	var lastUser map[string]any
	for _, m := range msgs {
		if m["role"] == "user" {
			lastUser = m
		}
	}
	if lastUser == nil || lastUser["content"] != "continue" {
		t.Fatalf("last user message: %v", lastUser)
	}
}

func TestRunRequiresPrompt(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	code, _, errOut := runMain(t, "", "run", "--dir", dir)
	if code != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "the following arguments are required: prompt") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestRunLoopErrorExit2(t *testing.T) {
	_, dir := setupCLI(t, lengthSSE, 200)
	code, _, errOut := runMain(t, "", "run", "Loop", "--dir", dir)
	if code != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "context overflow") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestRunGatewayErrorExit1(t *testing.T) {
	_, dir := setupCLI(t, "bad", 400)
	code, _, errOut := runMain(t, "", "run", "hi", "--dir", dir)
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "gateway HTTP 400") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestRunAgentFlagReachesLoop(t *testing.T) {
	f, dir := setupCLI(t, answerSSE, 200)
	code, _, _ := runMain(t, "", "run", "hi", "--dir", dir, "--agent", "Arjuna")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	system := ""
	for _, m := range f.wireMessages(0) {
		if m["role"] == "system" {
			system, _ = m["content"].(string)
		}
	}
	if !strings.Contains(system, "Arjuna") {
		t.Fatalf("persona missing from system prompt")
	}
}

func TestRunUnknownAgentExit1(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	code, _, errOut := runMain(t, "", "run", "hi", "--dir", dir, "--agent", "Bogus")
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "no such agent: Bogus") {
		t.Fatalf("err: %q", errOut)
	}
}

// -- batch ----------------------------------------------------------------------

func TestBatchTwoPrompts(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	batchFile := filepath.Join(dir, "batch.txt")
	_ = os.WriteFile(batchFile, []byte("first prompt\nsecond prompt\n"), 0o644)
	code, out, errOut := runMain(t, "", "run", "--batch", batchFile, "--dir", dir, "--workers", "1")
	if code != 0 {
		t.Fatalf("code %d err %q", code, errOut)
	}
	if strings.Count(out, "--- ") != 2 {
		t.Fatalf("out: %q", out)
	}
	if strings.Count(out, "Hello from the gateway.") != 2 {
		t.Fatalf("out: %q", out)
	}
	if len(sessions.ListSessions()) != 2 {
		t.Fatalf("sessions: %v", sessions.ListSessions())
	}
}

func TestBatchJSONArrayFile(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	batchFile := filepath.Join(dir, "batch.json")
	_ = os.WriteFile(batchFile, []byte("[\"one\", \"two\"]\n"), 0o644)
	code, out, errOut := runMain(t, "", "run", "--batch", batchFile, "--dir", dir, "--json", "--workers", "1")
	if code != 0 {
		t.Fatalf("code %d err %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var records []map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &records); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if len(records) != 2 {
		t.Fatalf("records: %d", len(records))
	}
	// Records are in file order.
	for i, want := range []string{"one", "two"} {
		prompts, _ := records[i]["answer"].(string)
		_ = prompts
		if records[i]["answer"] != "Hello from the gateway.\n" {
			t.Fatalf("record %d: %v", i, records[i])
		}
		_ = want
	}
	sessions := sessions.ListSessions()
	if len(sessions) != 2 {
		t.Fatalf("sessions: %d", len(sessions))
	}
}

func TestBatchAndPositionalMutuallyExclusive(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	code, _, errOut := runMain(t, "", "run", "prompt", "--batch", "x", "--dir", dir)
	if code != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "not allowed with argument prompt") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestBatchMissingFile(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	code, _, errOut := runMain(t, "", "run", "--batch", filepath.Join(dir, "nope.txt"), "--dir", dir)
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "cannot read batch file") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestBatchEmptyFile(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	batchFile := filepath.Join(dir, "empty.txt")
	_ = os.WriteFile(batchFile, []byte("  \n\n"), 0o644)
	code, _, errOut := runMain(t, "", "run", "--batch", batchFile, "--dir", dir)
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "contains no prompts") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestBatchWorkersZeroRejected(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	code, _, errOut := runMain(t, "", "run", "--batch", "x", "--dir", dir, "--workers", "0")
	if code != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "--workers must be at least 1") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestBatchLoopErrorExit2(t *testing.T) {
	_, dir := setupCLI(t, lengthSSE, 200)
	batchFile := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(batchFile, []byte("one\n"), 0o644)
	code, _, errOut := runMain(t, "", "run", "--batch", batchFile, "--dir", dir)
	if code != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "1 of 1 task(s) failed (1 loop)") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestBatchNonJSONSeparators(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	// Valid JSON but NOT a string array: falls back to line mode.
	batchFile := filepath.Join(dir, "nums.json")
	_ = os.WriteFile(batchFile, []byte("[1,\n2]"), 0o644)
	code, out, _ := runMain(t, "", "run", "--batch", batchFile, "--dir", dir, "--workers", "1")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if strings.Count(out, "Hello from the gateway.") != 2 { // "[1," and "2]" as prompts
		t.Fatalf("out: %q", out)
	}
}

// -- sessions --------------------------------------------------------------------

func TestSessionsShowDeletePrune(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KAAL_SESSIONS_DIR", filepath.Join(dir, "sessions"))
	sid := "20260802-000000-000001"
	_ = sessions.AppendEvent(sid, map[string]any{"type": "user", "data": map[string]any{"content": "hello"}})
	_ = sessions.AppendEvent(sid, map[string]any{"type": "assistant", "data": map[string]any{"content": "hi"}})

	code, out, _ := runMain(t, "", "sessions", "show", sid)
	if code != 0 || !strings.Contains(out, "| user |") {
		t.Fatalf("code %d out %q", code, out)
	}

	code, _, errOut := runMain(t, "", "sessions", "show", "nope")
	if code != 1 || !strings.Contains(errOut, "kaal: no such session: nope") {
		t.Fatalf("code %d err %q", code, errOut)
	}

	code, out, _ = runMain(t, "", "sessions", "delete", sid)
	if code != 0 || out != "deleted "+sid+"\n" {
		t.Fatalf("code %d out %q", code, out)
	}
	code, out, _ = runMain(t, "", "sessions", "delete", sid)
	if code != 1 || !strings.Contains(out, "no such session") {
		t.Fatalf("code %d out %q", code, out)
	}

	sid2 := "20260802-000000-000002"
	_ = sessions.AppendEvent(sid2, map[string]any{"type": "user", "data": map[string]any{"content": "x"}})
	code, out, _ = runMain(t, "", "sessions", "prune", "--keep", "0")
	if code != 0 || !strings.Contains(out, "deleted "+sid2) {
		t.Fatalf("code %d out %q", code, out)
	}
	code, out, _ = runMain(t, "", "sessions", "prune")
	if code != 0 || out != "nothing to prune\n" {
		t.Fatalf("code %d out %q", code, out)
	}
}

// -- doctor / update / diagrams ----------------------------------------------------

func TestDoctor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENCODE_API_KEY", "sk-test")
	t.Setenv("KAAL_SESSIONS_DIR", filepath.Join(dir, "sessions"))
	// Isolate the user config: tests must never touch the real key/model
	// stores.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer probe.Close()
	old := doctorGatewayURL
	doctorGatewayURL = probe.URL
	defer func() { doctorGatewayURL = old }()
	code, out, _ := runMain(t, "", "doctor")
	if code != 0 {
		t.Fatalf("code %d out %q", code, out)
	}
	for _, want := range []string{"go:", "api key: env", "gateway: reachable", "sessions dir:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestDoctorFailsWithoutKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("KAAL_SESSIONS_DIR", filepath.Join(dir, "sessions"))
	// Isolate config before pinning a keyed (paid) model: the free-tier
	// default is keyless by design and must NOT fail the doctor.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", t.TempDir()) // isolate the CLI auth file + omp fallbacks
	if err := config.SaveUserModel("deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer probe.Close()
	old := doctorGatewayURL
	doctorGatewayURL = probe.URL
	defer func() { doctorGatewayURL = old }()
	code, out, _ := runMain(t, "", "doctor")
	if code != 1 || !strings.Contains(out, "doctor: FAILED") {
		t.Fatalf("code %d out %q", code, out)
	}
	if !strings.Contains(out, "api key: MISSING") {
		t.Fatalf("out: %q", out)
	}
}

func TestUpdateNoCheckoutFallsBackToRelease(t *testing.T) {
	// A machine with no checkout (binary install) and no release published
	// yet: update must reach the release-fetch path and report the gap.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("KAAL_INSTALL_DIR", "")
	old := updateExecutable
	updateExecutable = func() string { return filepath.Join(dir, "kaal") }
	defer func() { updateExecutable = old }()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer apiSrv.Close()
	oldAPI := kaalReleaseAPI
	kaalReleaseAPI = apiSrv.URL
	defer func() { kaalReleaseAPI = oldAPI }()
	code, _, errOut := runMain(t, "", "update")
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "no release published yet") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3", "0.4", -1},
		{"0.4", "0.3", 1},
		{"0.3", "0.3", 0},
		{"0.10", "0.9", 1},
		{"1.0", "0.99", 1},
		{"0.3.1", "0.3", 1},
		{"0.3", "0.3.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestUpdateReleaseFetchesAndSwaps(t *testing.T) {
	skipOnWindows(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAAL_INSTALL_DIR", "")
	target := filepath.Join(home, "bin", "kaal")
	_ = os.MkdirAll(filepath.Join(home, "bin"), 0o755)
	_ = os.WriteFile(target, []byte("old binary\n"), 0o755)
	old := updateExecutable
	updateExecutable = func() string { return target }
	defer func() { updateExecutable = old }()

	// The release asset is an executable script that answers --version.
	assetName := releaseAssetName()
	assetBody := "#!/bin/sh\necho \"kaal 0.4\"\n"
	assetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(assetBody))
	}))
	defer assetSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := fmt.Sprintf(`{"tag_name":"v0.4","assets":[{"name":"%s","browser_download_url":"%s"}]}`,
			assetName, assetSrv.URL)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(meta))
	}))
	defer apiSrv.Close()
	oldAPI := kaalReleaseAPI
	kaalReleaseAPI = apiSrv.URL
	defer func() { kaalReleaseAPI = oldAPI }()

	code, out, errOut := runMain(t, "", "update")
	if code != 0 {
		t.Fatalf("code %d err %q", code, errOut)
	}
	if !strings.Contains(out, "kaal updated: 0.3 -> 0.4 (v0.4)") {
		t.Fatalf("out: %q", out)
	}
	if !strings.Contains(out, "restart kaal") {
		t.Fatalf("out: %q", out)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != assetBody {
		t.Fatalf("binary not swapped: %q", raw)
	}
}

func TestUpdateReleaseUpToDate(t *testing.T) {
	skipOnWindows(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAAL_INSTALL_DIR", "")
	target := filepath.Join(home, "bin", "kaal")
	_ = os.MkdirAll(filepath.Join(home, "bin"), 0o755)
	_ = os.WriteFile(target, []byte("old binary\n"), 0o755)
	old := updateExecutable
	updateExecutable = func() string { return target }
	defer func() { updateExecutable = old }()

	assetName := releaseAssetName()
	assetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("#!/bin/sh\necho \"kaal 0.3\"\n"))
	}))
	defer assetSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := fmt.Sprintf(`{"tag_name":"v0.3","assets":[{"name":"%s","browser_download_url":"%s"}]}`,
			assetName, assetSrv.URL)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(meta))
	}))
	defer apiSrv.Close()
	oldAPI := kaalReleaseAPI
	kaalReleaseAPI = apiSrv.URL
	defer func() { kaalReleaseAPI = oldAPI }()

	code, out, _ := runMain(t, "", "update")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out, "kaal is up to date (0.3)") {
		t.Fatalf("out: %q", out)
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "old binary\n" {
		t.Fatal("target must be untouched when up to date")
	}
}

func TestDiagramsMissingTermaidHints(t *testing.T) {
	t.Setenv("PATH", "")
	code, _, errOut := runMain(t, "", "diagrams", "x.mmd")
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "termaid not found") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestReadBatchPromptsModes(t *testing.T) {
	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "a.json")
	_ = os.WriteFile(jsonFile, []byte("[\"one\", \"two\"]"), 0o644)
	prompts, err := readBatchPrompts(jsonFile)
	if err != nil || len(prompts) != 2 || prompts[0] != "one" {
		t.Fatalf("prompts: %v err %v", prompts, err)
	}
	linesFile := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(linesFile, []byte("one\n\n two \n"), 0o644)
	prompts, err = readBatchPrompts(linesFile)
	if err != nil || len(prompts) != 2 || prompts[1] != "two" {
		t.Fatalf("prompts: %v err %v", prompts, err)
	}
	// Valid JSON but not a string array: falls back to lines.
	numsFile := filepath.Join(dir, "c.json")
	_ = os.WriteFile(numsFile, []byte("[1,\n2]"), 0o644)
	prompts, err = readBatchPrompts(numsFile)
	if err != nil || len(prompts) != 2 {
		t.Fatalf("prompts: %v err %v", prompts, err)
	}
}

func TestStructureEntryCount(t *testing.T) {
	if got := structureEntryCount([]byte("# x\nFiles: 3 · Dirs: 1\n")); got != 4 {
		t.Fatalf("got %d", got)
	}
	if got := structureEntryCount([]byte("no header")); got != 0 {
		t.Fatalf("got %d", got)
	}
}

func TestNoSubcommandNeedsTerminal(t *testing.T) {
	// A non-TTY stdout (the test buffer) must not launch the TUI: it gets
	// the one-shot hint and exit 1.
	code, _, errOut := runMain(t, "", "")
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut, "TUI needs a terminal") {
		t.Fatalf("err: %q", errOut)
	}
}

func TestRunUsesSavedDefaultModel(t *testing.T) {
	f, dir := setupCLI(t, answerSSE, 200)
	if err := config.SaveUserModel("kimi-k2.5"); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runMain(t, "", "run", "hi", "--dir", dir)
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(f.requests[0], &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "kimi-k2.5" {
		t.Fatalf("saved model not used: %q", body.Model)
	}
}

func TestRunModelFlagWins(t *testing.T) {
	f, dir := setupCLI(t, answerSSE, 200)
	if err := config.SaveUserModel("kimi-k2.5"); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runMain(t, "", "run", "hi", "--dir", dir, "--model", "hy3")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(f.requests[0], &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "hy3" {
		t.Fatalf("flag model not used: %q", body.Model)
	}
}

func TestRunMemoryRootFlag(t *testing.T) {
	_, dir := setupCLI(t, answerSSE, 200)
	memRoot := filepath.Join(dir, "custom-memory")
	code, _, _ := runMain(t, "", "run", "hi", "--dir", dir, "--memory-root", memRoot)
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(filepath.Join(memRoot, "project-state.md")); err != nil {
		t.Fatalf("memory root not used: %v", err)
	}
}

func TestUpdatePullsAndRebuilds(t *testing.T) {
	skipOnWindows(t)
	// A fake git checkout + fake git/go on PATH: update must pull, see a new
	// commit, and rebuild the Go binary in the checkout.
	home := t.TempDir()
	checkout := filepath.Join(home, "checkout")
	_ = os.MkdirAll(filepath.Join(checkout, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module github.com/kaal/kaal\n"), 0o644)
	t.Setenv("HOME", home)
	t.Setenv("KAAL_INSTALL_DIR", checkout)

	binDir := filepath.Join(home, "bin")
	_ = os.MkdirAll(binDir, 0o755)
	// Stateful fake git: the first rev-parse (before pull) reports
	// FAKE_BEFORE, later ones FAKE_AFTER.
	countFile := filepath.Join(home, "git-count")
	gitScript := `#!/bin/sh
case "$1" in
  rev-parse)
    if [ -f "$GIT_COUNT_FILE" ]; then echo "${FAKE_AFTER:-def456}"; else touch "$GIT_COUNT_FILE"; echo "${FAKE_BEFORE:-abc123}"; fi ;;
  pull) exit 0 ;;
  log) echo "the fake commit" ;;
esac
`
	_ = os.WriteFile(filepath.Join(binDir, "git"), []byte(gitScript), 0o755)
	// Fake go: a `build` invocation creates the binary in the checkout cwd.
	goScript := "#!/bin/sh\ncase \"$1\" in\n  build) touch kaal ;;\nesac\n"
	_ = os.WriteFile(filepath.Join(binDir, "go"), []byte(goScript), 0o755)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("GIT_COUNT_FILE", countFile)
	t.Setenv("FAKE_BEFORE", "abc123")
	t.Setenv("FAKE_AFTER", "def456")
	code, out, errOut := runMain(t, "", "update")
	if code != 0 {
		t.Fatalf("code %d err %q", code, errOut)
	}
	if !strings.Contains(out, "abc123 -> def456 (the fake commit)") {
		t.Fatalf("out: %q", out)
	}
	if !strings.Contains(out, "rebuilt at "+filepath.Join(checkout, "kaal")) {
		t.Fatalf("out: %q", out)
	}
	if _, err := os.Stat(filepath.Join(checkout, "kaal")); err != nil {
		t.Fatal("Go binary not rebuilt in the checkout")
	}
}

func TestUpdateUpToDate(t *testing.T) {
	skipOnWindows(t)
	home := t.TempDir()
	checkout := filepath.Join(home, "checkout")
	_ = os.MkdirAll(filepath.Join(checkout, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module github.com/kaal/kaal\n"), 0o644)
	t.Setenv("HOME", home)
	t.Setenv("KAAL_INSTALL_DIR", checkout)
	binDir := filepath.Join(home, "bin")
	_ = os.MkdirAll(binDir, 0o755)
	gitScript := `#!/bin/sh
case "$1" in
  rev-parse) echo "same123" ;;
  pull) exit 0 ;;
  log) echo "subject" ;;
esac
`
	_ = os.WriteFile(filepath.Join(binDir, "git"), []byte(gitScript), 0o755)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	code, out, _ := runMain(t, "", "update")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out, "up to date (same123)") {
		t.Fatalf("out: %q", out)
	}
}

func TestUpdateTarballFallbackOverlaysAndRebuilds(t *testing.T) {
	skipOnWindows(t)
	// No git on PATH: update fetches the main-branch tarball from the URL
	// seam and overlays it on the checkout; stale code dirs are cleared but
	// live files (AGENTS.md, docs/) must survive.
	home := t.TempDir()
	checkout := filepath.Join(home, "checkout")
	_ = os.MkdirAll(checkout, 0o755)
	_ = os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module github.com/kaal/kaal\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(checkout, "cmd"), 0o755)
	_ = os.WriteFile(filepath.Join(checkout, "cmd", "old.go"), []byte("stale\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(checkout, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(checkout, "docs", "keep.md"), []byte("keep\n"), 0o644)
	_ = os.WriteFile(filepath.Join(checkout, "AGENTS.md"), []byte("keep\n"), 0o644)
	t.Setenv("HOME", home)
	t.Setenv("KAAL_INSTALL_DIR", checkout)

	// Build the fake main-branch tarball: top dir + files.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, content string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	add("kaal-main/go.mod", "module github.com/kaal/kaal\n")
	add("kaal-main/newfile.txt", "fresh\n")
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	old := kaalUpdateURL
	kaalUpdateURL = srv.URL
	defer func() { kaalUpdateURL = old }()

	// go on PATH (the rebuild path), but NO git.
	binDir := filepath.Join(home, "bin")
	_ = os.MkdirAll(binDir, 0o755)
	_ = os.WriteFile(filepath.Join(binDir, "go"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", binDir)

	code, out, errOut := runMain(t, "", "update")
	if code != 0 {
		t.Fatalf("code %d err %q", code, errOut)
	}
	if !strings.Contains(out, "updated from the main tarball") {
		t.Fatalf("out: %q", out)
	}
	if !strings.Contains(out, "rebuilt at "+filepath.Join(checkout, "kaal")) {
		t.Fatalf("out: %q", out)
	}
	// Overlaid files present; stale cmd dir cleared; live files survived.
	if _, err := os.Stat(filepath.Join(checkout, "newfile.txt")); err != nil {
		t.Fatal("overlaid file missing")
	}
	if _, err := os.Stat(filepath.Join(checkout, "cmd")); err == nil {
		t.Fatal("stale cmd dir must be cleared")
	}
	if _, err := os.Stat(filepath.Join(checkout, "docs", "keep.md")); err != nil {
		t.Fatal("docs must survive")
	}
	if _, err := os.Stat(filepath.Join(checkout, "AGENTS.md")); err != nil {
		t.Fatal("AGENTS.md must survive")
	}
}

func TestResolveCheckoutAcceptsGitlessTarballDir(t *testing.T) {
	home := t.TempDir()
	checkout := filepath.Join(home, "checkout")
	_ = os.MkdirAll(checkout, 0o755)
	_ = os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module github.com/kaal/kaal\n"), 0o644)
	t.Setenv("HOME", home)
	t.Setenv("KAAL_INSTALL_DIR", checkout)
	if got := resolveCheckout(); got != checkout {
		t.Fatalf("gitless checkout: %q", got)
	}
}

func TestRunFreeTierKeyless(t *testing.T) {
	// The zen free tier needs no login: with every key source empty, a
	// free-tier model still answers.
	f, dir := setupCLI(t, answerSSE, 200)
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("CMD_API_KEY", "")
	t.Setenv("COMMANDCODE_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // isolate the omp fallback too

	code, out, errOut := runMain(t, "", "run", "Say hello", "--dir", dir)
	if code != 0 {
		t.Fatalf("code %d (stderr %q)", code, errOut)
	}
	if !strings.Contains(out, "Hello from the gateway.") {
		t.Fatalf("answer missing: %q", out)
	}

	// A paid model with no key anywhere must still fail fast.
	code, _, errOut = runMain(t, "", "run", "hi", "--dir", dir, "--model", "deepseek-v4-flash")
	if code != 1 || !strings.Contains(errOut, "no API key") {
		t.Fatalf("paid model keyless: code=%d err=%q", code, errOut)
	}
	_ = f
}

func TestBenchCommandJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"h\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	code, out, _ := runMain(t, "", "bench", "--json",
		"--base-url", srv.URL, "--model", "test-model",
		"--requests", "3", "--max-tokens", "20")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var doc struct {
		TTFT   []float64 `json:"ttft"`
		Total  []float64 `json:"total"`
		Failed int       `json:"failed"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if len(doc.TTFT) != 3 || len(doc.Total) != 3 || doc.Failed != 0 {
		t.Fatalf("samples: %+v", doc)
	}
	for i := range doc.TTFT {
		if doc.TTFT[i] <= 0 || doc.Total[i] < doc.TTFT[i] {
			t.Fatalf("sample %d: ttft=%v total=%v", i, doc.TTFT[i], doc.Total[i])
		}
	}

	// A failing endpoint (all requests error) must exit 1.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"nope"}}`)
	}))
	defer failing.Close()
	code, _, _ = runMain(t, "", "bench", "--json",
		"--base-url", failing.URL, "--model", "m", "--requests", "2")
	if code != 1 {
		t.Fatalf("all-failed bench code %d", code)
	}
}
