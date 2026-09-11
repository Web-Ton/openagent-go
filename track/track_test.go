package track

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
)

// ── Unit tests ──

func TestBuildEvent_FieldMapping(t *testing.T) {
	params := SessionParams{
		SessionID:   "sess-123",
		EntryPoint:  openagent.EntryPointACP,
		SessionMode: "manual",
		DurationMs:  5000,
		FailReason:  "test error",
	}
	evt := BuildEvent(EventSessionCreate, params)

	if evt.Event != EventSessionCreate {
		t.Errorf("Event = %q, want %q", evt.Event, EventSessionCreate)
	}
	// AnonymousId/DistinctId come from hostname, not SessionID.
	host, _ := os.Hostname()
	if evt.AnonymousId == "" {
		t.Error("AnonymousId should not be empty (hostname-derived)")
	}
	if evt.DistinctId != evt.AnonymousId {
		t.Errorf("DistinctId = %q, want %q (same as AnonymousId)", evt.DistinctId, evt.AnonymousId)
	}
	if evt.AnonymousId == params.SessionID {
		t.Errorf("AnonymousId should be hostname-derived, not SessionID %q", params.SessionID)
	}
	_ = host // host is informational; identity may be MD5(host) if not hex32
	if evt.Type != "track" {
		t.Errorf("Type = %q, want track", evt.Type)
	}
	if evt.Time <= 0 {
		t.Error("Time should be positive (UnixMilli)")
	}
	if evt.Properties.SessionID != "sess-123" {
		t.Errorf("Properties.SessionID = %q, want sess-123", evt.Properties.SessionID)
	}
	if evt.Properties.EntryPoint != "acp" {
		t.Errorf("Properties.EntryPoint = %q, want acp", evt.Properties.EntryPoint)
	}
	if evt.Properties.SessionMode != "manual" {
		t.Errorf("Properties.SessionMode = %q, want manual", evt.Properties.SessionMode)
	}
	if evt.Properties.DurationMs != 5000 {
		t.Errorf("Properties.DurationMs = %d, want 5000", evt.Properties.DurationMs)
	}
	if evt.Properties.FailReason != "test error" {
		t.Errorf("Properties.FailReason = %q, want 'test error'", evt.Properties.FailReason)
	}
	if evt.Properties.Time == "" {
		t.Error("Properties.Time should not be empty (RFC3339Nano)")
	}
}

func TestBuildEvent_AllThreeEventTypes(t *testing.T) {
	params := SessionParams{SessionID: "s1", EntryPoint: openagent.EntryPointCLI}

	create := BuildEvent(EventSessionCreate, params)
	closeEvt := BuildEvent(EventSessionClose, params)
	deleteEvt := BuildEvent(EventSessionDelete, params)

	if create.Event != "HwCloudCli_Session_Create" {
		t.Errorf("create event = %q, want HwCloudCli_Session_Create", create.Event)
	}
	if closeEvt.Event != "HwCloudCli_Session_Close" {
		t.Errorf("close event = %q, want HwCloudCli_Session_Close", closeEvt.Event)
	}
	if deleteEvt.Event != "HwCloudCli_Session_Delete" {
		t.Errorf("delete event = %q, want HwCloudCli_Session_Delete", deleteEvt.Event)
	}
}

func TestEncodeToBase64_RoundTrip(t *testing.T) {
	original := []EventReq{
		BuildEvent(EventSessionCreate, SessionParams{SessionID: "s1", EntryPoint: "acp"}),
	}
	encoded, err := encodeToBase64(original)
	if err != nil {
		t.Fatalf("encodeToBase64 failed: %v", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}

	var result []EventReq
	if err := json.Unmarshal(decoded, &result); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("decoded length = %d, want 1", len(result))
	}
	if result[0].Event != original[0].Event {
		t.Errorf("decoded event = %q, want %q", result[0].Event, original[0].Event)
	}
	if result[0].Properties.SessionID != original[0].Properties.SessionID {
		t.Errorf("decoded session_id = %q, want %q",
			result[0].Properties.SessionID, original[0].Properties.SessionID)
	}
}

func TestReportEvent_NoOpWhenUrlEmpty(t *testing.T) {
	// Save and restore state.
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	EventPostUrl = ""
	eventHttpClient = nil

	// Should return immediately without panicking or making network calls.
	evt := BuildEvent(EventSessionCreate, SessionParams{SessionID: "s1"})
	ReportEvent(context.Background(), evt) // must not block or panic
}

func TestReportEvent_NoOpWhenClientNil(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	EventPostUrl = "http://example.com/track"
	eventHttpClient = nil // client never initialized

	evt := BuildEvent(EventSessionCreate, SessionParams{SessionID: "s1"})
	ReportEvent(context.Background(), evt) // must not block or panic
}

func TestReportEvent_PanicSafe(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	// Set up a client pointing to an invalid URL to trigger an error path.
	EventPostUrl = "http://invalid-url-that-does-not-exist.invalid/track"
	eventHttpClient = &http.Client{Timeout: 1 * time.Second}

	// Should not panic even if the request fails.
	evt := BuildEvent(EventSessionCreate, SessionParams{SessionID: "s1"})
	ReportEvent(context.Background(), evt)
}

func TestInit_NoOpWhenUrlEmpty(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	EventPostUrl = ""
	eventHttpClient = nil
	Init()
	if eventHttpClient != nil {
		t.Error("Init should not create client when EventPostUrl is empty")
	}
}

func TestInit_Idempotent(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	EventPostUrl = "http://localhost:9999/track"
	eventHttpClient = nil
	Init()
	first := eventHttpClient
	Init() // second call should not replace the client
	if eventHttpClient != first {
		t.Error("Init should be idempotent (same client pointer)")
	}
}

// ── Observer tests ──

func TestObserver_DelegatesToReportEvent(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	// Use httptest to verify the Observer methods produce correct events.
	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	EventPostUrl = server.URL
	AppID = "test-app"
	eventHttpClient = server.Client()

	obs := &Observer{}
	obs.OnSessionCreate(context.Background(), openagent.SessionLifecycleEvent{
		SessionID:   "sess-obs-1",
		EntryPoint:  openagent.EntryPointACP,
		SessionMode: "plan",
	})

	// Decode the received form data.
	form, err := url.ParseQuery(receivedBody)
	if err != nil {
		t.Fatalf("ParseQuery failed: %v", err)
	}
	if form.Get("appid") != "test-app" {
		t.Errorf("appid = %q, want test-app", form.Get("appid"))
	}

	// Decode the Base64 JSON event.
	data := form.Get("data")
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	var events []EventReq
	if err := json.Unmarshal(decoded, &events); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Event != EventSessionCreate {
		t.Errorf("event = %q, want %q", events[0].Event, EventSessionCreate)
	}
	if events[0].Properties.SessionID != "sess-obs-1" {
		t.Errorf("session_id = %q, want sess-obs-1", events[0].Properties.SessionID)
	}
	if events[0].Properties.SessionMode != "plan" {
		t.Errorf("session_mode = %q, want plan", events[0].Properties.SessionMode)
	}
}

func TestObserver_OnSessionClose_IncludesDuration(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	EventPostUrl = server.URL
	AppID = "test-app"
	eventHttpClient = server.Client()

	obs := &Observer{}
	obs.OnSessionClose(context.Background(), openagent.SessionLifecycleEvent{
		SessionID:  "sess-obs-2",
		EntryPoint: openagent.EntryPointCLI,
		DurationMs: 12345,
	})

	form, _ := url.ParseQuery(receivedBody)
	decoded, _ := base64.StdEncoding.DecodeString(form.Get("data"))
	var events []EventReq
	if err := json.Unmarshal(decoded, &events); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if events[0].Event != EventSessionClose {
		t.Errorf("event = %q, want %q", events[0].Event, EventSessionClose)
	}
	if events[0].Properties.DurationMs != 12345 {
		t.Errorf("duration_ms = %d, want 12345", events[0].Properties.DurationMs)
	}
}

func TestObserver_OnSessionDelete_IncludesFailReason(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	EventPostUrl = server.URL
	AppID = "test-app"
	eventHttpClient = server.Client()

	obs := &Observer{}
	obs.OnSessionDelete(context.Background(), openagent.SessionLifecycleEvent{
		SessionID:  "sess-obs-3",
		EntryPoint: openagent.EntryPointACP,
		Err:        io.ErrUnexpectedEOF,
	})

	form, _ := url.ParseQuery(receivedBody)
	decoded, _ := base64.StdEncoding.DecodeString(form.Get("data"))
	var events []EventReq
	if err := json.Unmarshal(decoded, &events); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if events[0].Event != EventSessionDelete {
		t.Errorf("event = %q, want %q", events[0].Event, EventSessionDelete)
	}
	if !strings.Contains(events[0].Properties.FailReason, "unexpected EOF") {
		t.Errorf("fail_reason = %q, want it to contain 'unexpected EOF'",
			events[0].Properties.FailReason)
	}
}

// ── Integration test ──

func TestIntegration_ServerReceivesAllThreeEventTypes(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	savedAppID := AppID
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
		AppID = savedAppID
	}()

	var receivedPosts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedPosts = append(receivedPosts, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	EventPostUrl = server.URL
	AppID = "integration-test"
	eventHttpClient = server.Client()

	ctx := context.Background()
	obs := &Observer{}

	obs.OnSessionCreate(ctx, openagent.SessionLifecycleEvent{
		SessionID:  "s-int-1",
		EntryPoint: openagent.EntryPointACP,
		CreatedAt:  time.Now().Add(-10 * time.Second),
	})
	obs.OnSessionClose(ctx, openagent.SessionLifecycleEvent{
		SessionID:  "s-int-1",
		EntryPoint: openagent.EntryPointACP,
		DurationMs: 10000,
	})
	obs.OnSessionDelete(ctx, openagent.SessionLifecycleEvent{
		SessionID:  "s-int-2",
		EntryPoint: openagent.EntryPointREST,
	})

	if len(receivedPosts) != 3 {
		t.Fatalf("expected 3 POSTs, got %d", len(receivedPosts))
	}

	// Decode each POST and verify the event type.
	wantEvents := []string{EventSessionCreate, EventSessionClose, EventSessionDelete}
	for i, body := range receivedPosts {
		form, err := url.ParseQuery(body)
		if err != nil {
			t.Fatalf("POST %d: ParseQuery failed: %v", i, err)
		}
		if form.Get("appid") != "integration-test" {
			t.Errorf("POST %d: appid = %q, want integration-test", i, form.Get("appid"))
		}
		decoded, _ := base64.StdEncoding.DecodeString(form.Get("data"))
		var events []EventReq
		if err := json.Unmarshal(decoded, &events); err != nil {
			t.Fatalf("POST %d: unmarshal failed: %v", i, err)
		}
		if len(events) != 1 {
			t.Fatalf("POST %d: expected 1 event, got %d", i, len(events))
		}
		if events[0].Event != wantEvents[i] {
			t.Errorf("POST %d: event = %q, want %q", i, events[0].Event, wantEvents[i])
		}
	}
}

// ── Tool-call event tests ──

func TestBuildToolCallEvent_FieldMapping(t *testing.T) {
	params := ToolCallParams{
		ToolName:   "list_deployments",
		EntryPoint: openagent.EntryPointIAC,
		DurationMs: 42,
		FailReason: "boom",
	}
	evt := BuildToolCallEvent(EventToolCall, params)

	if evt.Event != EventToolCall {
		t.Errorf("Event = %q, want %q", evt.Event, EventToolCall)
	}
	if evt.Event != "IacMcpServer_Tool_Call" {
		t.Errorf("Event literal = %q, want IacMcpServer_Tool_Call", evt.Event)
	}
	if evt.Type != "track" {
		t.Errorf("Type = %q, want track", evt.Type)
	}
	if evt.Properties.ToolName != "list_deployments" {
		t.Errorf("ToolName = %q, want list_deployments", evt.Properties.ToolName)
	}
	if evt.Properties.EntryPoint != "iac" {
		t.Errorf("EntryPoint = %q, want iac", evt.Properties.EntryPoint)
	}
	if evt.Properties.DurationMs != 42 {
		t.Errorf("DurationMs = %d, want 42", evt.Properties.DurationMs)
	}
	if evt.Properties.FailReason != "boom" {
		t.Errorf("FailReason = %q, want boom", evt.Properties.FailReason)
	}
	// A tool call has no session semantics — these MUST be empty.
	if evt.Properties.SessionID != "" {
		t.Errorf("SessionID = %q, want empty (tool call has no session)", evt.Properties.SessionID)
	}
	if evt.Properties.SessionMode != "" {
		t.Errorf("SessionMode = %q, want empty (tool call has no session)", evt.Properties.SessionMode)
	}
	// Identity is hostname-derived (same as BuildEvent), not the tool name.
	if evt.AnonymousId == "" {
		t.Error("AnonymousId should not be empty (hostname-derived)")
	}
	if evt.DistinctId != evt.AnonymousId {
		t.Errorf("DistinctId = %q, want same as AnonymousId %q", evt.DistinctId, evt.AnonymousId)
	}
}

// TestBuildEvent_Session_NoToolNameLeak is a regression guard: adding the
// ToolName field to properties must NOT pollute session-event JSON.  A
// session event built via BuildEvent should serialize without a "tool_name"
// key (omitempty on the empty value).
func TestBuildEvent_Session_NoToolNameLeak(t *testing.T) {
	evt := BuildEvent(EventSessionCreate, SessionParams{
		SessionID:  "s1",
		EntryPoint: openagent.EntryPointACP,
	})
	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props, ok := raw["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties missing or wrong type")
	}
	if _, present := props["tool_name"]; present {
		t.Errorf("session event JSON contains tool_name (should be omitted via omitempty): %s", string(data))
	}
}

func TestReportEvent_ToolCall(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	savedAppID := AppID
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
		AppID = savedAppID
	}()

	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	EventPostUrl = srv.URL
	AppID = "test-app"
	eventHttpClient = srv.Client()

	ReportEvent(context.Background(), BuildToolCallEvent(EventToolCall, ToolCallParams{
		ToolName:   "apply_deployment",
		EntryPoint: openagent.EntryPointIAC,
		DurationMs: 123,
	}))

	form, err := url.ParseQuery(receivedBody)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if form.Get("appid") != "test-app" {
		t.Errorf("appid = %q, want test-app", form.Get("appid"))
	}
	decoded, err := base64.StdEncoding.DecodeString(form.Get("data"))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	var events []EventReq
	if err := json.Unmarshal(decoded, &events); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Event != EventToolCall {
		t.Errorf("event = %q, want %q", events[0].Event, EventToolCall)
	}
	if events[0].Properties.ToolName != "apply_deployment" {
		t.Errorf("tool_name = %q, want apply_deployment", events[0].Properties.ToolName)
	}
	if events[0].Properties.DurationMs != 123 {
		t.Errorf("duration_ms = %d, want 123", events[0].Properties.DurationMs)
	}
}

func TestToolCallObserverImpl_Delegates(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	savedAppID := AppID
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
		AppID = savedAppID
	}()

	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	EventPostUrl = srv.URL
	AppID = "test-app"
	eventHttpClient = srv.Client()

	obs := GetToolCallObserver()
	if obs == nil {
		t.Fatal("GetToolCallObserver returned nil")
	}
	obs.OnToolCall(context.Background(), openagent.ToolCallEvent{
		ToolName:   "propose_architecture",
		EntryPoint: openagent.EntryPointIAC,
		DurationMs: 500,
		Err:        nil,
	})

	form, _ := url.ParseQuery(receivedBody)
	decoded, _ := base64.StdEncoding.DecodeString(form.Get("data"))
	var events []EventReq
	if err := json.Unmarshal(decoded, &events); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Event != EventToolCall {
		t.Errorf("event = %q, want %q", events[0].Event, EventToolCall)
	}
	if events[0].Properties.ToolName != "propose_architecture" {
		t.Errorf("tool_name = %q, want propose_architecture", events[0].Properties.ToolName)
	}
	if events[0].Properties.FailReason != "" {
		t.Errorf("fail_reason = %q, want empty (Err was nil)", events[0].Properties.FailReason)
	}
}

// TestToolCallObserverImpl_DelegatesWithError verifies FailReason is
// populated from ToolCallEvent.Err.
func TestToolCallObserverImpl_DelegatesWithError(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	savedAppID := AppID
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
		AppID = savedAppID
	}()

	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	EventPostUrl = srv.URL
	AppID = "test-app"
	eventHttpClient = srv.Client()

	obs := GetToolCallObserver()
	obs.OnToolCall(context.Background(), openagent.ToolCallEvent{
		ToolName:   "destroy_deployment",
		EntryPoint: openagent.EntryPointIAC,
		DurationMs: 1,
		Err:        errors.New("permission denied"),
	})

	form, _ := url.ParseQuery(receivedBody)
	decoded, _ := base64.StdEncoding.DecodeString(form.Get("data"))
	var events []EventReq
	if err := json.Unmarshal(decoded, &events); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if events[0].Properties.FailReason != "permission denied" {
		t.Errorf("fail_reason = %q, want 'permission denied'", events[0].Properties.FailReason)
	}
}

// TestToolCallObserverImpl_NoOpWhenDisabled verifies the observer is a
// no-op (no HTTP call, no panic) when tracking is disabled — the dev/e2e
// build default (empty EventPostUrl).
func TestToolCallObserverImpl_NoOpWhenDisabled(t *testing.T) {
	savedUrl := EventPostUrl
	savedClient := eventHttpClient
	defer func() {
		EventPostUrl = savedUrl
		eventHttpClient = savedClient
	}()

	EventPostUrl = "" // disabled
	eventHttpClient = nil

	obs := GetToolCallObserver()
	// Must not panic and must not attempt any network call.
	obs.OnToolCall(context.Background(), openagent.ToolCallEvent{
		ToolName:   "list_deployments",
		EntryPoint: openagent.EntryPointIAC,
		DurationMs: 1,
	})
}

