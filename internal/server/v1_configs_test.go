package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/server"
	"github.com/tphakala/alfred/internal/server/apiv1gen"
)

// sampleWorkflowYAML is a minimal valid workflow YAML for tests.
const sampleWorkflowYAML = "name: triage\nmodel: gemini\nsteps: []\n"

// contentTypeJSON is the media type expected on successful JSON responses.
const contentTypeJSON = "application/json"

// newConfigTestServer creates a test server with WorkflowsDir set to dir.
func newConfigTestServer(t *testing.T, dir string) *server.Server {
	t.Helper()
	return newTestServer(t, &server.Deps{WorkflowsDir: dir})
}

// doPutWithHeaders issues an authenticated PUT request with a JSON body and
// extra headers, returning the recorded response.
func doPutWithHeaders(
	t *testing.T,
	srv *server.Server,
	path, body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", contentTypeJSON)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestListWorkflowConfigs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte(sampleWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newConfigTestServer(t, dir)
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var list apiv1gen.WorkflowConfigList
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(list.Items))
	}
	item := list.Items[0]
	if item.Name != "triage" {
		t.Errorf("Name = %q, want %q", item.Name, "triage")
	}
	if item.Etag == nil || *item.Etag == "" {
		t.Error("Etag is nil or empty in list response")
	}
}

func TestListWorkflowConfigs_EmptyDir(t *testing.T) {
	srv := newConfigTestServer(t, t.TempDir())
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var list apiv1gen.WorkflowConfigList
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if list.Items == nil {
		t.Error("Items must be a non-nil slice even for an empty directory")
	}
	if len(list.Items) != 0 {
		t.Errorf("len(Items) = %d, want 0", len(list.Items))
	}
}

func TestListWorkflowConfigs_NoDirConfigured(t *testing.T) {
	srv := newConfigTestServer(t, "")
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var list apiv1gen.WorkflowConfigList
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if list.Items == nil {
		t.Error("Items must be non-nil even when WorkflowsDir is empty")
	}
}

func TestGetWorkflowConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte(sampleWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newConfigTestServer(t, dir)
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs/triage")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var cfg apiv1gen.WorkflowConfig
	if err := json.NewDecoder(rr.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if cfg.Content == nil {
		t.Fatal("Content is nil")
	}
	if cfg.Etag == nil || *cfg.Etag == "" {
		t.Fatal("Etag is nil or empty")
	}
	// ETag response header must match the body etag field.
	headerEtag := rr.Header().Get("ETag")
	if headerEtag != *cfg.Etag {
		t.Errorf("ETag header %q != body etag %q", headerEtag, *cfg.Etag)
	}
}

func TestGetWorkflowConfig_NotFound(t *testing.T) {
	srv := newConfigTestServer(t, t.TempDir())
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs/nope")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}

func TestPutWorkflowConfig_UpdateWithMatchingEtag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte(sampleWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newConfigTestServer(t, dir)

	// Get existing config to obtain its etag.
	getResp := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs/triage")
	if getResp.Code != http.StatusOK {
		t.Fatalf("pre-GET status = %d; body: %s", getResp.Code, getResp.Body.String())
	}
	var initial apiv1gen.WorkflowConfig
	if err := json.NewDecoder(getResp.Body).Decode(&initial); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	etag, ok := initial.Etag, initial.Etag != nil
	if !ok || *etag == "" {
		t.Fatal("initial etag is nil or empty")
	}

	newContent := "name: triage\nmodel: claude\nsteps: []\n"
	body := `{"name":"triage","content":` + string(mustMarshalJSON(t, newContent)) + `}`
	rr := doPutWithHeaders(t, srv, "/api/v1/workflow-configs/triage", body,
		map[string]string{"If-Match": *etag})

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// GET should now return the new content with a different etag.
	getResp2 := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs/triage")
	if getResp2.Code != http.StatusOK {
		t.Fatalf("post-GET status = %d", getResp2.Code)
	}
	var updated apiv1gen.WorkflowConfig
	if err := json.NewDecoder(getResp2.Body).Decode(&updated); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if updated.Content == nil || *updated.Content != newContent {
		t.Errorf("Content = %v, want %q", updated.Content, newContent)
	}
	if updated.Etag == nil || *updated.Etag == *etag {
		t.Error("etag after PUT should differ from the original etag")
	}
}

func TestPutWorkflowConfig_WrongEtagReturns412(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte(sampleWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newConfigTestServer(t, dir)
	body := `{"name":"triage","content":"name: triage\nmodel: gemini\nsteps: []\n"}`
	rr := doPutWithHeaders(t, srv, "/api/v1/workflow-configs/triage", body,
		map[string]string{"If-Match": `"deadbeef"`})

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusPreconditionFailed, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}

func TestPutWorkflowConfig_MissingEtagOnExistingFileReturns412(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte(sampleWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newConfigTestServer(t, dir)
	body := `{"name":"triage","content":"name: triage\nmodel: gemini\nsteps: []\n"}`
	// No If-Match header; existing file means this is a 412.
	rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/workflow-configs/triage", body)

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusPreconditionFailed, rr.Body.String())
	}
}

func TestPutWorkflowConfig_CreateNew(t *testing.T) {
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	newContent := "name: newcfg\nmodel: gemini\nsteps: []\n"
	body := `{"name":"newcfg","content":` + string(mustMarshalJSON(t, newContent)) + `}`
	// No If-Match; file does not exist, so creation is allowed.
	rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/workflow-configs/newcfg", body)

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	// Verify file was created on disk.
	diskData, err := os.ReadFile(filepath.Join(dir, "newcfg.yaml"))
	if err != nil {
		t.Fatalf("file not created on disk: %v", err)
	}
	if string(diskData) != newContent {
		t.Errorf("disk content = %q, want %q", string(diskData), newContent)
	}

	// GET should return the new config.
	getResp := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs/newcfg")
	if getResp.Code != http.StatusOK {
		t.Fatalf("GET after PUT status = %d", getResp.Code)
	}
}

func TestPutWorkflowConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	body := `{"name":"bad","content":":\n  bad: ["}`
	rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/workflow-configs/bad", body)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}

func TestPutWorkflowConfig_NameMismatchRejected(t *testing.T) {
	// The YAML body's internal name: must match the URL {name}. A mismatch
	// would let an operator write triage.yaml whose name: is "other"; if a
	// second file also declares "other", the #89 loader dup-name guard then
	// fails the entire load and bricks the next restart. The write path must
	// reject the mismatch rather than manufacture a loader-rejected state.
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	body := `{"name":"triage","content":` + string(mustMarshalJSON(t, "name: other\nsteps: []\n")) + `}`
	rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/workflow-configs/triage", body)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
	// A rejected write must not have created the file on disk.
	if _, err := os.ReadFile(filepath.Join(dir, "triage.yaml")); !os.IsNotExist(err) {
		t.Errorf("triage.yaml must not exist after a rejected mismatched PUT; ReadFile err = %v", err)
	}
}

func TestPutWorkflowConfig_TraversalGuard(t *testing.T) {
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	// "foo%2Fbar" is URL-decoded by the mux to "foo/bar"; filepath.Base("foo/bar")
	// returns "bar", so safeConfigName rejects it as a path traversal attempt.
	body := `{"name":"foo/bar","content":"name: x\nsteps: []\n"}`
	rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/workflow-configs/foo%2Fbar", body)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

func TestPutWorkflowConfig_WildcardOnExistingFileSucceeds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte(sampleWorkflowYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newConfigTestServer(t, dir)
	newContent := "name: triage\nmodel: claude\nsteps: []\n"
	body := `{"name":"triage","content":` + string(mustMarshalJSON(t, newContent)) + `}`
	// If-Match: * matches any existing resource.
	rr := doPutWithHeaders(t, srv, "/api/v1/workflow-configs/triage", body,
		map[string]string{"If-Match": "*"})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestPutWorkflowConfig_WildcardOnNonExistentFileReturns412(t *testing.T) {
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	body := `{"name":"newcfg","content":"name: newcfg\nmodel: gemini\nsteps: []\n"}`
	// If-Match: * requires the resource to exist, so creation must be rejected.
	rr := doPutWithHeaders(t, srv, "/api/v1/workflow-configs/newcfg", body,
		map[string]string{"If-Match": "*"})

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusPreconditionFailed, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}

// mustMarshalJSON marshals v to JSON, failing the test on error.
func mustMarshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// sampleAgentConfigs returns a minimal agent config slice for agent-task tests.
func sampleAgentConfigs() []agentcfg.AgentConfig {
	return []agentcfg.AgentConfig{
		{
			Name:     "sentry-triage",
			Strategy: "fanout",
			Runner:   "claude",
			Schedule: agentcfg.Schedule{Cron: "0 * * * *"},
		},
	}
}

func TestPutWorkflowConfig_AtomicWriteLeavesNoTempFile(t *testing.T) {
	// The atomic write goes through a .tmp file that must be renamed over the
	// target, never left behind. A lingering .atomic-*.tmp is ignored by the
	// .yaml-only loader, but it must not accumulate on every write.
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	create := `{"name":"triage","content":` + string(mustMarshalJSON(t, "name: triage\nmodel: a\nsteps: []\n")) + `}`
	if rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/workflow-configs/triage", create); rr.Code != http.StatusOK {
		t.Fatalf("create status = %d; body %s", rr.Code, rr.Body.String())
	}
	// Update as well, so the rename-over-existing path is exercised too.
	get := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs/triage")
	update := `{"name":"triage","content":` + string(mustMarshalJSON(t, "name: triage\nmodel: b\nsteps: []\n")) + `}`
	if rr := doPutWithHeaders(t, srv, "/api/v1/workflow-configs/triage", update, map[string]string{"If-Match": get.Header().Get("ETag")}); rr.Code != http.StatusOK {
		t.Fatalf("update status = %d; body %s", rr.Code, rr.Body.String())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "triage.yaml" {
		t.Errorf("WorkflowsDir = %v, want exactly [triage.yaml] with no temp litter", names)
	}
}

func TestPutWorkflowConfig_ConcurrentSameEtagWritesSerialized(t *testing.T) {
	// Several PUTs racing with the SAME If-Match etag must not all win: the
	// config write lock makes the read-check-write one critical section, so
	// exactly one commits (200) and the rest see the now-changed etag and get
	// 412. Without the lock they all read the stale etag, pass the precondition,
	// and clobber each other (all 200). Runs under -race.
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte("name: triage\nmodel: v0\nsteps: []\n"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	get := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs/triage")
	if get.Code != http.StatusOK {
		t.Fatalf("GET seed status = %d", get.Code)
	}
	etag := get.Header().Get("ETag")
	if etag == "" {
		t.Fatal("seed ETag is empty")
	}

	const n = 8
	// Precompute the request bodies on the test goroutine: mustMarshalJSON calls
	// t.Fatalf, which is undefined behavior from a non-test goroutine.
	contents := make([]string, n)
	bodies := make([]string, n)
	for i := range n {
		contents[i] = "name: triage\nmodel: v" + strconv.Itoa(i+1) + "\nsteps: []\n"
		bodies[i] = `{"name":"triage","content":` + string(mustMarshalJSON(t, contents[i])) + `}`
	}
	codes := make([]int, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/v1/workflow-configs/triage", strings.NewReader(bodies[i]))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			req.Header.Set("Content-Type", contentTypeJSON)
			req.Header.Set("If-Match", etag)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			codes[i] = rr.Code
		}(i)
	}
	wg.Wait()

	ok, pre, winner := 0, 0, -1
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
			winner = i
		case http.StatusPreconditionFailed:
			pre++
		default:
			t.Errorf("unexpected status %d, want 200 or 412", c)
		}
	}
	if ok != 1 {
		t.Errorf("exactly one PUT must commit, got %d with 200 and %d with 412", ok, pre)
	}
	if pre != n-1 {
		t.Errorf("the other %d PUTs must get 412, got %d", n-1, pre)
	}

	// The committed file must be intact and byte-identical to the WINNER's body
	// (the goroutine that got 200), never a torn or interleaved mix.
	data, err := os.ReadFile(filepath.Join(dir, "triage.yaml"))
	if err != nil {
		t.Fatalf("reading final config: %v", err)
	}
	if winner >= 0 && string(data) != contents[winner] {
		t.Errorf("final config = %q, want the winner's body %q", data, contents[winner])
	}
}

func TestPutWorkflowConfig_WhitespaceNameRejected(t *testing.T) {
	// A whitespace-only URL name ("%20" -> " ") passes the base/dot path checks
	// but would persist a " .yaml" whose whitespace-only internal name is rejected
	// by LoadWorkflow on the next boot, failing the all-or-nothing load. The write
	// path must reject it (400) and write nothing.
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	body := `{"name":" ","content":` + string(mustMarshalJSON(t, "name: \" \"\nsteps: []\n")) + `}`
	rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/workflow-configs/%20", body)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("WorkflowsDir must be empty after a rejected whitespace-name PUT, got %v", entries)
	}
}

func TestPutWorkflowConfig_WriteFailureLeavesTargetIntact(t *testing.T) {
	// When the atomic write fails (here the workflows dir is made read-only so
	// CreateTemp cannot create the temp file), the handler returns 500, the
	// existing config is left untouched, and no .atomic-*.tmp orphan remains.
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	dir := t.TempDir()
	srv := newConfigTestServer(t, dir)

	original := "name: triage\nmodel: original\nsteps: []\n"
	if err := os.WriteFile(filepath.Join(dir, "triage.yaml"), []byte(original), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	get := doRequest(t, srv, http.MethodGet, "/api/v1/workflow-configs/triage")
	etag := get.Header().Get("ETag")

	// Read-only dir: the existing file is still readable (etag check passes), but
	// CreateTemp needs dir write and fails, so fsutil.WriteFileAtomic errors.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // ensure t.TempDir cleanup can remove it

	update := `{"name":"triage","content":` + string(mustMarshalJSON(t, "name: triage\nmodel: updated\nsteps: []\n")) + `}`
	rr := doPutWithHeaders(t, srv, "/api/v1/workflow-configs/triage", update, map[string]string{"If-Match": etag})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusInternalServerError, rr.Body.String())
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "triage.yaml"))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if string(data) != original {
		t.Errorf("existing config must be untouched on write failure, got %q", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "triage.yaml" {
		t.Errorf("dir must contain only triage.yaml with no temp orphan, got %v", entries)
	}
}

func TestListAgentTasksV1(t *testing.T) {
	cfgs := sampleAgentConfigs()
	srv := newTestServer(t, &server.Deps{AgentConfigs: cfgs})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/agent-tasks")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}

	var list apiv1gen.AgentTaskList
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(list.Items))
	}
	item := list.Items[0]
	if item.Name != "sentry-triage" {
		t.Errorf("Name = %q, want %q", item.Name, "sentry-triage")
	}
	if item.Runner == nil || *item.Runner != "claude" {
		t.Errorf("Runner = %v, want %q", item.Runner, "claude")
	}
	if item.Strategy == nil || *item.Strategy != "fanout" {
		t.Errorf("Strategy = %v, want %q", item.Strategy, "fanout")
	}
	if item.Schedule == nil || *item.Schedule != "0 * * * *" {
		t.Errorf("Schedule = %v, want %q", item.Schedule, "0 * * * *")
	}
}

func TestListAgentTasksV1_Empty(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/agent-tasks")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var list apiv1gen.AgentTaskList
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if list.Items == nil {
		t.Error("Items must be non-nil even when AgentConfigs is empty")
	}
	if len(list.Items) != 0 {
		t.Errorf("len(Items) = %d, want 0", len(list.Items))
	}
}

func TestGetAgentTaskV1(t *testing.T) {
	cfgs := sampleAgentConfigs()
	srv := newTestServer(t, &server.Deps{AgentConfigs: cfgs})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/agent-tasks/sentry-triage")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}

	var task apiv1gen.AgentTask
	if err := json.NewDecoder(rr.Body).Decode(&task); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if task.Name != "sentry-triage" {
		t.Errorf("Name = %q, want %q", task.Name, "sentry-triage")
	}
	if task.Runner == nil || *task.Runner != "claude" {
		t.Errorf("Runner = %v, want %q", task.Runner, "claude")
	}
	if task.Strategy == nil || *task.Strategy != "fanout" {
		t.Errorf("Strategy = %v, want %q", task.Strategy, "fanout")
	}
	if task.Schedule == nil || *task.Schedule != "0 * * * *" {
		t.Errorf("Schedule = %v, want %q", task.Schedule, "0 * * * *")
	}
}

func TestGetAgentTaskV1_NotFound(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/agent-tasks/nope")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}

func TestRunAgentTaskV1_NotFound(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/agent-tasks/nope/run", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}

func TestRunAgentTaskV1_NoTemporalClient(t *testing.T) {
	cfgs := sampleAgentConfigs()
	srv := newTestServer(t, &server.Deps{AgentConfigs: cfgs})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/agent-tasks/sentry-triage/run", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusServiceUnavailable, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}
