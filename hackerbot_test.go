package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProgramsPaginationAndAuthentication(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		username, password, ok := r.BasicAuth()
		if !ok || username != "alice" || password != "secret" {
			t.Errorf("unexpected basic authentication")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page[number]") == "2" {
			fmt.Fprint(w, `{"data":[{"id":"2","type":"program","attributes":{"handle":"two"}}],"links":{"next":null}}`)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"1","type":"program","attributes":{"handle":"one"}}],"links":{"next":%q}}`, serverURL(r)+"/v1/hackers/programs?page%5Bnumber%5D=2")
	}))
	defer server.Close()

	client := newH1Client(testConfig(server.URL))
	programs, err := client.Programs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(programs) != 2 || calls.Load() != 2 {
		t.Fatalf("got %d programs in %d calls", len(programs), calls.Load())
	}
}

func TestLimitedProgramListReadsOnlyFirstPageInAPIReturnOrder(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.URL.Query().Get("page[size]"); got != "3" {
			t.Errorf("page size = %q, want 3", got)
		}
		fmt.Fprint(w, `{"data":[
			{"id":"1","type":"program","attributes":{"handle":"top"}},
			{"id":"2","type":"program","attributes":{"handle":"middle"}},
			{"id":"3","type":"program","attributes":{"handle":"bottom"}}
		],"links":{"next":"https://example.invalid/must-not-be-followed"}}`)
	}))
	defer server.Close()

	programs, err := newH1Client(testConfig(server.URL)).ProgramsLimited(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	var handles []string
	for _, program := range programs {
		handle, err := programHandle(program)
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, handle)
	}
	if calls.Load() != 1 || strings.Join(handles, ",") != "top,middle,bottom" {
		t.Fatalf("limited list made %d calls and returned %v", calls.Load(), handles)
	}
}

func TestHackerOneClientUsesOnlyGET(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method != http.MethodGet {
			http.Error(w, "mutation refused", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hackers/programs", "/v1/hackers/programs/acme/structured_scopes", "/v1/hackers/programs/acme/scope_exclusions", "/v1/hackers/me/reports":
			fmt.Fprint(w, `{"data":[],"links":{}}`)
		case "/v1/hackers/programs/acme":
			fmt.Fprint(w, `{"data":{"id":"1","type":"program","attributes":{"handle":"acme"}}}`)
		case "/v1/hackers/reports/42":
			fmt.Fprint(w, `{"data":{"id":"42","type":"report","attributes":{"state":"new"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newH1Client(testConfig(server.URL))
	if _, err := client.Programs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Program(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Scopes(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ScopeExclusions(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Reports(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Report(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
	for _, method := range methods {
		if !strings.HasPrefix(method, "GET ") {
			t.Fatalf("HackerOne mutation request observed: %s", method)
		}
	}
}

func TestHackerOneClientRejectsCrossOriginPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[],"links":{"next":"https://example.invalid/v1/stolen"}}`)
	}))
	defer server.Close()
	_, err := newH1Client(testConfig(server.URL)).Programs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outside configured API origin") {
		t.Fatalf("cross-origin pagination error = %v", err)
	}
}

func TestHackerOneClientRejectsCustomerAPI(t *testing.T) {
	client := newH1Client(testConfig("https://api.hackerone.com"))
	_, err := client.resolveTarget("/v1/programs/1989/bounty_table")
	if err == nil || !strings.Contains(err.Error(), "/v1/hackers") {
		t.Fatalf("customer API path was not rejected: %v", err)
	}
}

func TestConfigRejectsUnsafeAPIOriginAndRateIntervals(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		general    string
		report     string
		scope      string
		wantPhrase string
	}{
		{"wrong origin", "https://example.com", "150ms", "210ms", "1250ms", "must be exactly"},
		{"general too fast", "https://api.hackerone.com", "100ms", "210ms", "1250ms", "at least 110ms"},
		{"reports too fast", "https://api.hackerone.com", "150ms", "200ms", "1250ms", "at least 210ms"},
		{"scopes too fast", "https://api.hackerone.com", "150ms", "210ms", "1200ms", "at least 1250ms"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			body := fmt.Sprintf(`{"hackerone_base_url":%q,"hackerone_username":"id","hackerone_api_token":"token","state_file":"state.db","request_interval":%q,"report_request_interval":%q,"scope_request_interval":%q}`, test.baseURL, test.general, test.report, test.scope)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.wantPhrase) {
				t.Fatalf("loadConfig error = %v, want %q", err, test.wantPhrase)
			}
		})
	}
}

func TestObjectAcceptsBareJSONAPIResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1989","type":"program","attributes":{"handle":"starbucks"},"relationships":{}}`)
	}))
	defer server.Close()

	program, err := newH1Client(testConfig(server.URL)).Program(context.Background(), "starbucks")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := programHandle(program)
	if err != nil {
		t.Fatal(err)
	}
	if handle != "starbucks" {
		t.Fatalf("handle = %q", handle)
	}
}

func TestFirstReportCheckIsSilentAndActivityChangeSends(t *testing.T) {
	var revision atomic.Int32
	revision.Store(1)
	var discordCalls atomic.Int32
	var discordPayload atomic.Value

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hackers/me/reports":
			fmt.Fprintf(w, `{"data":[{"id":"42","type":"report","attributes":{"title":"XSS","state":"new","last_activity_at":"2026-01-0%dT00:00:00Z"}}],"links":{}}`, revision.Load())
		case "/v1/hackers/reports/42":
			if revision.Load() == 1 {
				fmt.Fprint(w, `{"data":{"id":"42","type":"report","attributes":{"title":"XSS","state":"new","last_activity_at":"2026-01-01T00:00:00Z"},"relationships":{"activities":{"data":[]}}}}`)
			} else {
				fmt.Fprint(w, `{"data":{"id":"42","type":"report","attributes":{"title":"XSS","state":"triaged","last_activity_at":"2026-01-02T00:00:00Z"},"relationships":{"activities":{"data":[{"id":"9","type":"activity-comment","attributes":{"message":"Please retest","created_at":"2026-01-02T00:00:00Z"}}]}}}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discordCalls.Add(1)
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("parse report webhook: %v", err)
		}
		payload := r.FormValue("payload_json")
		if previousPayload, ok := discordPayload.Load().(string); ok {
			payload = previousPayload + "\n" + payload
		}
		discordPayload.Store(payload)
		if len(r.MultipartForm.File) != 0 {
			t.Errorf("summary report webhook unexpectedly contained attachments")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()

	cfg := testConfig(api.URL)
	cfg.ReportWebhookURL = discord.URL
	cfg.ReportNotificationMode = "summary"
	cfg.StateFile = filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	monitor := newMonitor(cfg, store)
	if err := monitor.CheckReports(context.Background()); err != nil {
		t.Fatal(err)
	}
	if discordCalls.Load() != 0 {
		t.Fatalf("first report run sent %d webhooks", discordCalls.Load())
	}
	revision.Store(2)
	if err := monitor.CheckReports(context.Background()); err != nil {
		t.Fatal(err)
	}
	if discordCalls.Load() != 2 {
		t.Fatalf("changed report run sent %d webhooks, want separate status and comment cards", discordCalls.Load())
	}
	payload, _ := discordPayload.Load().(string)
	for _, expected := range []string{"### 🔄 Status changed", "**Status:** `New` → `Triaged`", "### 💬 New comment", "**Report:** [#42]", "**Content:** _Hidden in summary mode_"} {
		if !strings.Contains(payload, expected) {
			t.Errorf("summary payload missing %q: %s", expected, payload)
		}
	}
	if strings.Contains(payload, "Please retest") || strings.Contains(payload, "XSS") {
		t.Fatalf("summary payload disclosed report content: %s", payload)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state.Reports["42"].Report), "Please retest") {
		t.Fatal("updated report activity was not persisted")
	}
}

func TestProgramDiffIncludesPolicyAndScope(t *testing.T) {
	before := ProgramSnapshot{
		Program: json.RawMessage(`{"id":"1","attributes":{"handle":"acme","policy":"old"}}`),
		Scopes:  []json.RawMessage{json.RawMessage(`{"id":"10","attributes":{"asset_identifier":"old.example"}}`)},
	}
	after := ProgramSnapshot{
		Program: json.RawMessage(`{"id":"1","attributes":{"handle":"acme","policy":"new"}}`),
		Scopes: []json.RawMessage{
			json.RawMessage(`{"id":"10","attributes":{"asset_identifier":"new.example"}}`),
			json.RawMessage(`{"id":"11","attributes":{"asset_identifier":"added.example"}}`),
		},
	}
	details := diffProgram(before, after)
	paths := make([]string, 0, len(details))
	for _, detail := range details {
		paths = append(paths, detail.Path)
	}
	joined := strings.Join(paths, "\n")
	for _, expected := range []string{"program.attributes.policy", "scope[10].attributes.asset_identifier", "scope[11]"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %q in paths:\n%s", expected, joined)
		}
	}
}

func TestProgramDiffPreservesLongValues(t *testing.T) {
	oldPolicy := strings.Repeat("old policy line 🌍\n", 1000)
	newPolicy := strings.Repeat("new policy line 🔐\n", 1000)
	beforeRaw, err := json.Marshal(map[string]any{"attributes": map[string]any{"policy": oldPolicy}})
	if err != nil {
		t.Fatal(err)
	}
	afterRaw, err := json.Marshal(map[string]any{"attributes": map[string]any{"policy": newPolicy}})
	if err != nil {
		t.Fatal(err)
	}
	details := diffProgram(ProgramSnapshot{Program: beforeRaw}, ProgramSnapshot{Program: afterRaw})
	if len(details) != 1 || details[0].Before != oldPolicy || details[0].After != newPolicy {
		t.Fatal("long policy values were truncated during change detection")
	}
}

func TestProgramHTMLEscapesAPIContent(t *testing.T) {
	program, err := json.Marshal(map[string]any{
		"id": "1",
		"attributes": map[string]any{
			"handle": "acme",
			"name":   "Acme",
			"policy": `<script>alert("policy")</script>`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ProgramSnapshot{Handle: "acme", Program: program, CapturedAt: time.Now().UTC()}
	report, err := programHTML(ProgramChange{Kind: "new", Handle: "acme", After: &snapshot})
	if err != nil {
		t.Fatal(err)
	}
	htmlText := string(report)
	if strings.Contains(htmlText, `<script>alert("policy")</script>`) {
		t.Fatal("API-controlled HTML was not escaped")
	}
	if !strings.Contains(htmlText, `&lt;script&gt;alert`) {
		t.Fatal("escaped policy content is missing")
	}
}

func TestProgramHTMLScopeUsesSeverityOrderAndWideAssets(t *testing.T) {
	severities := []struct {
		id       string
		severity string
		asset    string
	}{
		{"1", "none", "none.example"},
		{"2", "low", "low.example"},
		{"3", "critical", "critical.example"},
		{"4", "medium", "medium.example"},
		{"5", "high", "high.example"},
	}
	snapshot := ProgramSnapshot{Handle: "acme", Program: json.RawMessage(`{"id":"1","attributes":{"handle":"acme"}}`)}
	for _, item := range severities {
		snapshot.Scopes = append(snapshot.Scopes, json.RawMessage(fmt.Sprintf(`{"id":%q,"attributes":{"asset_type":"URL","asset_identifier":%q,"max_severity":%q,"eligible_for_bounty":true,"eligible_for_submission":true}}`, item.id, item.asset, item.severity)))
	}
	report, err := programHTML(ProgramChange{Kind: "manual", Handle: "acme", After: &snapshot})
	if err != nil {
		t.Fatal(err)
	}
	htmlText := string(report)
	last := -1
	for _, asset := range []string{"critical.example", "high.example", "medium.example", "low.example", "none.example"} {
		position := strings.Index(htmlText, `<span class="asset">`+asset+`</span>`)
		if position <= last {
			t.Fatalf("asset %s is missing or out of severity order", asset)
		}
		last = position
	}
	for _, expected := range []string{"Asset identifier", "Critical, High, Medium, Low, then None", "white-space:nowrap", "min-width:310px"} {
		if !strings.Contains(htmlText, expected) {
			t.Fatalf("scope layout is missing %q", expected)
		}
	}
}

func TestFirstProgramCheckIsSilentAndSecondSends(t *testing.T) {
	var policy atomic.Int32
	policy.Store(1)
	var discordCalls atomic.Int32

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hackers/programs":
			fmt.Fprint(w, `{"data":[{"id":"1","type":"program","attributes":{"handle":"acme"}}],"links":{}}`)
		case "/v1/hackers/programs/acme":
			fmt.Fprintf(w, `{"data":{"id":"1","type":"program","attributes":{"handle":"acme","name":"Acme","policy":"v%d"}}}`, policy.Load())
		case "/v1/hackers/programs/acme/structured_scopes":
			fmt.Fprint(w, `{"data":[{"id":"1","type":"structured-scope","attributes":{"asset_type":"URL","asset_identifier":"https://example.com","eligible_for_submission":true,"eligible_for_bounty":true}}],"links":{}}`)
		case "/v1/hackers/programs/acme/scope_exclusions":
			fmt.Fprint(w, `{"data":[],"links":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discordCalls.Add(1)
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("parse multipart webhook: %v", err)
		}
		if r.FormValue("payload_json") == "" {
			t.Error("missing payload_json")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()

	cfg := testConfig(api.URL)
	cfg.ProgramWebhookURL = discord.URL
	cfg.StateFile = filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	monitor := newMonitor(cfg, store)
	if err := monitor.CheckPrograms(context.Background()); err != nil {
		t.Fatal(err)
	}
	if discordCalls.Load() != 0 {
		t.Fatalf("first run sent %d webhooks", discordCalls.Load())
	}
	policy.Store(2)
	if err := monitor.CheckPrograms(context.Background()); err != nil {
		t.Fatal(err)
	}
	if discordCalls.Load() != 2 {
		t.Fatalf("second run sent %d webhooks, want summary then HTML", discordCalls.Load())
	}
	stateBytes, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(stateBytes), "SQLite format 3") {
		t.Fatalf("state file is not SQLite: %q", stateBytes[:min(len(stateBytes), 16)])
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state.Programs["acme"].Program), `"policy":"v2"`) {
		t.Fatalf("program state was not advanced: %s", state.Programs["acme"].Program)
	}
}

func TestLegacyJSONStateMigratesToSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := `{"version":1,"programs_initialized":true,"reports_initialized":false,"programs":{"acme":{"handle":"acme","program":{"id":"1","attributes":{"handle":"acme"}},"scopes":[],"scope_exclusions":[],"captured_at":"2026-01-01T00:00:00Z"}},"reports":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !state.ProgramsInitialized || state.Programs["acme"].Handle != "acme" {
		t.Fatalf("legacy state was not imported: %#v", state)
	}
	if _, err := os.Stat(path + ".json-backup"); err != nil {
		t.Fatalf("legacy backup was not retained: %v", err)
	}
}

func TestSQLiteProgramRemovedAfterTwoCompleteMisses(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	program := ProgramSnapshot{Handle: "acme", Program: json.RawMessage(`{"id":"1","attributes":{"handle":"acme"}}`)}
	if err := store.InitializePrograms(map[string]ProgramSnapshot{"acme": program}); err != nil {
		t.Fatal(err)
	}
	removed, err := store.UpdateProgramPresence(map[string]ProgramSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("program removed after only one miss: %v", removed)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.MissingPrograms["acme"] != 1 {
		t.Fatalf("missing count = %d, want 1", state.MissingPrograms["acme"])
	}
	removed, err = store.UpdateProgramPresence(map[string]ProgramSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "acme" {
		t.Fatalf("second miss removed %v, want acme", removed)
	}
}

func TestReportSummaryIncludesNonCommentActivity(t *testing.T) {
	before := ReportSnapshot{Report: json.RawMessage(`{"id":"42","attributes":{"state":"triaged"},"relationships":{"activities":{"data":[]}}}`)}
	after := ReportSnapshot{Report: json.RawMessage(`{"id":"42","attributes":{"state":"triaged"},"relationships":{"activities":{"data":[{"id":"8","type":"activity-comments-closed"}]}}}`)}
	messages := reportSummaryMessages(ReportChange{Kind: "changed", ID: "42", Before: &before, After: &after})
	if len(messages) != 1 || !strings.Contains(messages[0], "### ⚡ Comments closed") {
		t.Fatalf("non-comment activity was not summarized: %v", messages)
	}
}

func TestDetailedReportEventsAndOwnCommentSwitch(t *testing.T) {
	before := ReportSnapshot{Report: json.RawMessage(`{
		"id":"42",
		"attributes":{"title":"Markdown *XSS*","state":"new"},
		"relationships":{
			"reporter":{"data":{"id":"hacker-1","type":"user","attributes":{"username":"alice"}}},
			"activities":{"data":[]}
		}
	}`)}
	after := ReportSnapshot{Report: json.RawMessage(`{
		"id":"42",
		"attributes":{"title":"Markdown *XSS*","state":"triaged"},
		"relationships":{
			"reporter":{"data":{"id":"hacker-1","type":"user","attributes":{"username":"alice"}}},
			"activities":{"data":[
				{"id":"1","type":"activity-comment","attributes":{"message":"my private follow-up"},"relationships":{"actor":{"data":{"id":"hacker-1","type":"user","attributes":{"username":"alice"}}}}},
				{"id":"2","type":"activity-comment","attributes":{"message":"**Please retest** _now_"},"relationships":{"actor":{"data":{"id":"team-1","type":"user","attributes":{"username":"security-team"}}}}},
				{"id":"3","type":"activity-bounty-awarded","attributes":{"amount":500,"currency":"USD"},"relationships":{"actor":{"data":{"id":"team-1","type":"user","attributes":{"username":"security-team"}}}}},
				{"id":"4","type":"activity-bug-triaged","attributes":{}}
			]}
		}
	}`)}
	change := ReportChange{Kind: "changed", ID: "42", Before: &before, After: &after}
	messages := reportNotificationMessages(change, "detailed", false)
	joined := strings.Join(messages, "\n")
	for _, expected := range []string{
		"### 🔄 Status changed",
		"**Status:** `New` → `Triaged`",
		"**Please retest** _now_",
		"### ⚡ Bounty awarded",
		"**Amount:** 500",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("detailed events missing %q:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "my private follow-up") {
		t.Fatalf("own comment was not suppressed:\n%s", joined)
	}
	if strings.Count(joined, "triaged") != 0 {
		// The human-facing status is title-cased; a second lower-case occurrence
		// would be the duplicate activity notification.
		t.Fatalf("state transition activity was sent twice:\n%s", joined)
	}

	withOwn := strings.Join(reportNotificationMessages(change, "detailed", true), "\n")
	if !strings.Contains(withOwn, "my private follow-up") {
		t.Fatalf("own comment switch did not include the reporter's comment:\n%s", withOwn)
	}
}

func TestDetailedNewReportOnlySendsIDAndTitle(t *testing.T) {
	after := ReportSnapshot{Report: json.RawMessage(`{
		"id":"77",
		"attributes":{"title":"Secret issue","state":"new","vulnerability_information":"do not send"},
		"relationships":{"activities":{"data":[{"id":"1","type":"activity-comment","attributes":{"message":"do not send this either"}}]}}
	}`)}
	messages := reportNotificationMessages(ReportChange{Kind: "new", ID: "77", After: &after}, "detailed", true)
	if len(messages) != 1 || !strings.Contains(messages[0], "#77") || !strings.Contains(messages[0], "Secret issue") {
		t.Fatalf("new report message = %v", messages)
	}
	if strings.Contains(messages[0], "do not send") {
		t.Fatalf("new report disclosed more than ID and title: %s", messages[0])
	}
}

func TestReportStateChangeCreatesAndReusesForumThread(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	var payloads []discordPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse Discord webhook: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		var payload discordPayload
		if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
			t.Errorf("decode Discord payload: %v", err)
		}
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		payloads = append(payloads, payload)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"message-1","channel_id":"thread-42"}`)
	}))
	defer server.Close()

	client := newDiscordClient(5 * time.Second)
	before := ReportSnapshot{Report: json.RawMessage(`{"attributes":{"title":"XSS","state":"new"},"relationships":{"activities":{"data":[]}}}`)}
	after := ReportSnapshot{Report: json.RawMessage(`{"attributes":{"title":"XSS","state":"triaged"},"relationships":{"activities":{"data":[{"id":"1","type":"activity-bug-triaged"}]}}}`)}
	threadID, err := client.SendReport(context.Background(), server.URL, ReportChange{Kind: "changed", ID: "42", Before: &before, After: &after}, "detailed", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if threadID != "thread-42" {
		t.Fatalf("created thread ID = %q", threadID)
	}

	before = after
	after = ReportSnapshot{DiscordThreadID: threadID, Report: json.RawMessage(`{"attributes":{"title":"XSS","state":"resolved"},"relationships":{"activities":{"data":[{"id":"1","type":"activity-bug-triaged"},{"id":"2","type":"activity-bug-resolved"}]}}}`)}
	if _, err := client.SendReport(context.Background(), server.URL, ReportChange{Kind: "changed", ID: "42", Before: &before, After: &after}, "detailed", false, true); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 {
		t.Fatalf("webhook calls = %d, want 2", len(payloads))
	}
	if payloads[0].ThreadName == "" || !strings.Contains(payloads[0].Content, "Triaged") {
		t.Fatalf("first state change did not create a thread: %#v", payloads[0])
	}
	if strings.Contains(queries[0], "thread_id=") {
		t.Fatalf("thread-creation request unexpectedly targeted a thread: %s", queries[0])
	}
	if payloads[1].ThreadName != "" || !strings.Contains(queries[1], "thread_id=thread-42") || !strings.Contains(payloads[1].Content, "Resolved") {
		t.Fatalf("second state change did not reuse the thread: query=%s payload=%#v", queries[1], payloads[1])
	}
}

func TestDummyReportSendsExistingActivitiesOldestToNewest(t *testing.T) {
	var mu sync.Mutex
	var contents []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse Discord webhook: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		var payload discordPayload
		if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
			t.Errorf("decode Discord payload: %v", err)
		}
		mu.Lock()
		contents = append(contents, payload.Content)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	report := json.RawMessage(`{
		"id":"42","type":"report","attributes":{"title":"Live preview","state":"triaged"},
		"relationships":{
			"reporter":{"data":{"id":"reporter","type":"user"}},
			"activities":{"data":[
				{"id":"later","type":"activity-comment","attributes":{"message":"later message","created_at":"2026-01-02T00:00:00Z"},"relationships":{"actor":{"data":{"id":"program","type":"user"}}}},
				{"id":"earlier","type":"activity-comment","attributes":{"message":"earlier message","created_at":"2026-01-01T00:00:00Z"},"relationships":{"actor":{"data":{"id":"program","type":"user"}}}}
			]}
		}
	}`)
	cfg := Config{
		ReportWebhookURL:       server.URL,
		ReportNotificationMode: "detailed",
		timeout:                5 * time.Second,
	}
	if err := sendDummyReport(context.Background(), newDiscordClient(cfg.timeout), cfg, "42", json.RawMessage(`{"id":"42"}`), report); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(contents) != 3 {
		t.Fatalf("Discord messages = %d, want preview plus two activities: %v", len(contents), contents)
	}
	if !strings.Contains(contents[0], "Live report test") || !strings.Contains(contents[1], "earlier message") || !strings.Contains(contents[2], "later message") {
		t.Fatalf("activity order = %v", contents)
	}
}

func TestSummaryShowsSeverityTargetWithoutTitleOrCommentBody(t *testing.T) {
	before := ReportSnapshot{Report: json.RawMessage(`{
		"attributes":{"title":"Private report title","state":"triaged"},
		"relationships":{
			"reporter":{"data":{"id":"researcher","type":"user"}},
			"severity":{"data":{"id":"1","type":"severity","attributes":{"rating":"medium"}}},
			"activities":{"data":[]}
		}
	}`)}
	after := ReportSnapshot{Report: json.RawMessage(`{
		"attributes":{"title":"Private report title","state":"triaged"},
		"relationships":{
			"reporter":{"data":{"id":"researcher","type":"user"}},
			"severity":{"data":{"id":"2","type":"severity","attributes":{"rating":"high"}}},
			"activities":{"data":[
				{"id":"1","type":"activity-severity-updated","attributes":{"old_severity":"medium","new_severity":"high"}},
				{"id":"2","type":"activity-comment","attributes":{"message":"Private comment body"},"relationships":{"actor":{"data":{"id":"program-member","type":"user","attributes":{"username":"triager"}}}}}
			]}
		}
	}`)}
	messages := reportNotificationMessages(ReportChange{Kind: "changed", ID: "42", Before: &before, After: &after}, "summary", false)
	joined := strings.Join(messages, "\n")
	for _, expected := range []string{"### 🟠 Severity changed", "**Severity:** `Medium` → `High`", "### 💬 New comment", "**Author:** triager", "Hidden in summary mode"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("summary missing %q:\n%s", expected, joined)
		}
	}
	for _, forbidden := range []string{"Private report title", "Private comment body", "Old severity", "New severity"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("summary disclosed or duplicated %q:\n%s", forbidden, joined)
		}
	}
	if strings.Count(joined, "Severity changed") != 1 {
		t.Fatalf("severity transition was sent more than once:\n%s", joined)
	}
}

func TestDummyLivePreviewFetchesThreeProgramsAndTwoReportsWithoutStateDB(t *testing.T) {
	var apiCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hackers/programs":
			if got := r.URL.Query().Get("page[size]"); got != "3" {
				t.Errorf("program page size = %q", got)
			}
			fmt.Fprint(w, `{"data":[
				{"id":"1","type":"program","attributes":{"handle":"one"}},
				{"id":"2","type":"program","attributes":{"handle":"two"}},
				{"id":"3","type":"program","attributes":{"handle":"three"}}
			],"links":{"next":"/v1/hackers/programs?page[number]=2"}}`)
		case "/v1/hackers/programs/one", "/v1/hackers/programs/two", "/v1/hackers/programs/three":
			handle := strings.TrimPrefix(r.URL.Path, "/v1/hackers/programs/")
			fmt.Fprintf(w, `{"data":{"id":%q,"type":"program","attributes":{"handle":%q,"name":%q,"policy":"Test policy"}}}`, handle, handle, "Program "+handle)
		case "/v1/hackers/programs/one/structured_scopes", "/v1/hackers/programs/two/structured_scopes", "/v1/hackers/programs/three/structured_scopes",
			"/v1/hackers/programs/one/scope_exclusions", "/v1/hackers/programs/two/scope_exclusions", "/v1/hackers/programs/three/scope_exclusions":
			fmt.Fprint(w, `{"data":[],"links":{}}`)
		case "/v1/hackers/me/reports":
			if got := r.URL.Query().Get("page[size]"); got != "2" {
				t.Errorf("report page size = %q", got)
			}
			fmt.Fprint(w, `{"data":[{"id":"10","type":"report"},{"id":"20","type":"report"}],"links":{"next":"/v1/hackers/me/reports?page[number]=2"}}`)
		case "/v1/hackers/reports/10", "/v1/hackers/reports/20":
			id := strings.TrimPrefix(r.URL.Path, "/v1/hackers/reports/")
			fmt.Fprintf(w, `{"data":{"id":%q,"type":"report","attributes":{"title":%q,"state":"triaged"},"relationships":{"activities":{"data":[]}}}}`, id, "Report "+id)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	var discordCalls atomic.Int32
	var dividerCalls atomic.Int32
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discordCalls.Add(1)
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("parse Discord preview request: %v", err)
		} else if strings.Contains(r.FormValue("payload_json"), "━━━━━━━━") {
			dividerCalls.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()

	statePath := filepath.Join(t.TempDir(), "must-not-exist.db")
	cfg := testConfig(api.URL)
	cfg.ProgramWebhookURL = discord.URL
	cfg.ReportWebhookURL = discord.URL
	cfg.ReportNotificationMode = "detailed"
	cfg.StateFile = statePath
	if err := sendDummyNotifications(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// 1 program list + (detail, scope, exclusions) * 3 + 1 report list + 2 details.
	if apiCalls.Load() != 13 {
		t.Fatalf("HackerOne calls = %d, want 13", apiCalls.Load())
	}
	// Each program uses an ordered summary+HTML pair, two dividers separate the
	// three programs, and each activity-free report has one preview message.
	if discordCalls.Load() != 10 || dividerCalls.Load() != 2 {
		t.Fatalf("Discord calls/dividers = %d/%d, want 10/2", discordCalls.Load(), dividerCalls.Load())
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("dummy preview created or accessed its state path: %v", err)
	}
}

func TestLoadDotEnvAndPreserveProcessEnvironment(t *testing.T) {
	const (
		newKey      = "HACKERBOT_TEST_DOTENV_NEW"
		existingKey = "HACKERBOT_TEST_DOTENV_EXISTING"
		urlKey      = "HACKERBOT_TEST_DOTENV_URL"
	)
	for _, key := range []string{newKey, existingKey, urlKey} {
		old, existed := os.LookupEnv(key)
		_ = os.Unsetenv(key)
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	if err := os.Setenv(existingKey, "from-process"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\nexport " + newKey + "='from file'\n" + existingKey + "=from-file\n" + urlKey + "=https://example.test/hook#token # trailing comment\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path, true); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(newKey); got != "from file" {
		t.Fatalf("new variable = %q", got)
	}
	if got := os.Getenv(existingKey); got != "from-process" {
		t.Fatalf("process environment was overridden: %q", got)
	}
	if got := os.Getenv(urlKey); got != "https://example.test/hook#token" {
		t.Fatalf("URL variable = %q", got)
	}
}

func TestOptionalDotEnvMayBeMissing(t *testing.T) {
	if err := loadDotEnv(filepath.Join(t.TempDir(), "missing.env"), false); err != nil {
		t.Fatal(err)
	}
}

func TestReportConfigSwitchesAndEnvironmentOverrides(t *testing.T) {
	t.Setenv("HACKERBOT_REPORTS_ENABLED", "false")
	t.Setenv("HACKERBOT_REPORT_NOTIFY_OWN_COMMENTS", "true")
	t.Setenv("HACKERBOT_REPORT_THREADS_ENABLED", "true")
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"hackerone_base_url":"https://api.hackerone.com",
		"hackerone_username":"alice",
		"hackerone_api_token":"secret",
		"state_file":"state.db",
		"report_notify_own_comments":false,
		"report_threads_enabled":false
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReportsEnabled || !cfg.ReportNotifyOwnComments || !cfg.ReportThreadsEnabled {
		t.Fatalf("report switches were not overridden: enabled=%v own=%v threads=%v", cfg.ReportsEnabled, cfg.ReportNotifyOwnComments, cfg.ReportThreadsEnabled)
	}
}

func TestDiscordProgramMessageIsOrderedAndUncutDataIsAttached(t *testing.T) {
	var mu sync.Mutex
	var requestKinds []string
	var filenames []string
	fileData := make(map[string][]byte)
	var payloads []discordPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hasFile := false
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("create multipart reader: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("read multipart: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			data, readErr := io.ReadAll(part)
			if readErr != nil {
				t.Errorf("read multipart part: %v", readErr)
				return
			}
			mu.Lock()
			if part.FileName() == "" && part.FormName() == "payload_json" {
				var payload discordPayload
				if err := json.Unmarshal(data, &payload); err != nil {
					t.Errorf("decode Discord payload: %v", err)
				}
				payloads = append(payloads, payload)
			} else if part.FileName() != "" {
				hasFile = true
				filenames = append(filenames, part.FileName())
				fileData[part.FileName()] = data
			}
			mu.Unlock()
		}
		mu.Lock()
		if !hasFile {
			requestKinds = append(requestKinds, "summary")
		} else {
			requestKinds = append(requestKinds, "attachment")
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	policy := strings.Repeat("Policy section 🌟\n", 1600)
	program, err := json.Marshal(map[string]any{
		"id":   "1",
		"type": "program",
		"attributes": map[string]any{
			"handle":           "acme",
			"name":             "Acme",
			"policy":           policy,
			"submission_state": "open",
			"state":            "public_mode",
			"offers_bounties":  true,
			"currency":         "usd",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := strings.Repeat("old value 🔐 ", 500)
	after := strings.Repeat("new value ✨ ", 500)
	details := []ChangeDetail{
		{Path: "program.attributes.policy", Before: before, After: after},
		{Path: "scope[10].attributes.asset_identifier", Before: "old.example", After: "new.example"},
		{Path: "scope[10].attributes.max_severity", Before: "high", After: "critical"},
		{Path: "scope[11]", Before: "(not present)", After: `{"asset_identifier":"added.example"}`},
		{Path: "program.attributes.fast_payments", Before: "false", After: "true"},
	}
	snapshot := ProgramSnapshot{
		Handle:     "acme",
		Program:    program,
		CapturedAt: time.Date(2026, 9, 8, 17, 59, 6, 0, time.UTC),
		Scopes: []json.RawMessage{
			json.RawMessage(`{"id":"10","attributes":{"asset_identifier":"new.example","eligible_for_bounty":true,"eligible_for_submission":true,"max_severity":"critical"}}`),
		},
	}
	client := newDiscordClient(5 * time.Second)
	err = client.SendProgram(context.Background(), server.URL, ProgramChange{Kind: "changed", Handle: "acme", After: &snapshot, Details: details})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	const htmlFilename = "acme-program-20260908T175906Z.html"
	wantOrder := []string{htmlFilename}
	if strings.Join(filenames, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("attachment order = %v, want %v", filenames, wantOrder)
	}
	htmlReport := string(fileData[htmlFilename])
	for _, completeValue := range []string{policy, before, after, "new.example"} {
		if !strings.Contains(htmlReport, completeValue) {
			t.Fatal("HTML report did not preserve complete policy, change, or scope data")
		}
	}
	sectionOrder := []string{`id="overview"`, `id="changes"`, `id="guidelines"`, `id="scope"`, `id="exclusions"`, `id="raw"`}
	lastPosition := -1
	for _, section := range sectionOrder {
		position := strings.Index(htmlReport, section)
		if position <= lastPosition {
			t.Fatalf("HTML section %s is missing or out of order", section)
		}
		lastPosition = position
	}
	if strings.Join(requestKinds, ",") != "summary,attachment" {
		t.Fatalf("Discord request order = %v, want summary then attachment", requestKinds)
	}
	if len(payloads) != 2 {
		t.Fatalf("payload count = %d, want 2", len(payloads))
	}
	if len(payloads[0].Embeds) != 2 || len(payloads[1].Embeds) != 0 {
		t.Fatalf("summary/attachment embeds = %d/%d, want 2/0", len(payloads[0].Embeds), len(payloads[1].Embeds))
	}
	totalText := 0
	for _, embed := range payloads[0].Embeds {
		if len([]rune(embed.Title)) > 256 || len([]rune(embed.Description)) > 4096 || len(embed.Fields) > 25 {
			t.Fatalf("embed exceeds Discord limits: %#v", embed)
		}
		totalText += len([]rune(embed.Title)) + len([]rune(embed.Description))
		if embed.Footer != nil {
			totalText += len([]rune(embed.Footer.Text))
		}
		for _, field := range embed.Fields {
			if len([]rune(field.Name)) > 256 || len([]rune(field.Value)) > 1024 {
				t.Fatalf("field exceeds Discord limits: %#v", field)
			}
			totalText += len([]rune(field.Name)) + len([]rune(field.Value))
		}
	}
	if totalText > 6000 {
		t.Fatalf("total embed text = %d, exceeds Discord limit", totalText)
	}
	if !strings.Contains(payloads[0].Embeds[1].Fields[0].Value, "Full value in `"+htmlFilename+"`") {
		t.Fatalf("long preview did not explain where the full value is: %s", payloads[0].Embeds[1].Fields[0].Value)
	}
	if strings.Contains(htmlReport, `id="rewards"`) || strings.Contains(htmlReport, `href="#rewards"`) {
		t.Fatal("HTML report still contains the unavailable bounty-table section")
	}
}

func testConfig(baseURL string) Config {
	return Config{
		HackerOneBaseURL:  baseURL,
		HackerOneUsername: "alice",
		HackerOneAPIToken: "secret",
		requestDelay:      time.Nanosecond,
		scopeDelay:        time.Nanosecond,
		timeout:           5 * time.Second,
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
