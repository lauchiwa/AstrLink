package providerapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/internal/providerapi"
)

func copilotRequest(t *testing.T, method, path, body string, header http.Header) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "https://api.githubcopilot.com"+path, strings.NewReader(body))
	for name, values := range header {
		request.Header[name] = values
	}
	if err := providerapi.CopilotRequest(request); err != nil {
		t.Fatalf("CopilotRequest(%s %s) = %v", method, path, err)
	}
	return request
}

func TestCopilotRequestMapsPathsAndKeepsOnlyOpenCodeHeaders(t *testing.T) {
	client := http.Header{
		"Authorization":          {"Bearer gho_secret"},
		"User-Agent":             {"opencode/1.18.34"},
		"X-Github-Api-Version":   {"2026-06-01"},
		"Openai-Intent":          {"conversation-edits"},
		"Accept":                 {"text/event-stream"},
		"Content-Type":           {"application/json; charset=utf-8"},
		"Anthropic-Beta":         {"interleaved-thinking-2025-05-14"},
		"X-Stainless-Lang":       {"js"},
		"Originator":             {"codex_cli_rs"},
		"Session_id":             {"client-session"},
		"X-Claude-Code-Session":  {"claude-session"},
		"Copilot-Integration-Id": {"vscode-chat"},
	}
	for _, tt := range []struct{ path, want, body string }{
		{"/v1/chat/completions", "/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", "/responses", `{"model":"gpt-5.4","input":"hi"}`},
		{"/v1/messages", "/v1/messages", `{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"hi"}]}`},
	} {
		request := copilotRequest(t, http.MethodPost, tt.path, tt.body, client)
		if request.URL.Path != tt.want || request.URL.Host != "api.githubcopilot.com" {
			t.Fatalf("%s -> %s", tt.path, request.URL)
		}
		allowed := map[string]bool{
			"Authorization": true, "User-Agent": true, "X-Github-Api-Version": true, "Openai-Intent": true,
			"Accept": true, "Content-Type": true, "X-Initiator": true, "X-Interaction-Id": true,
		}
		if tt.want == "/v1/messages" {
			allowed["Anthropic-Beta"], allowed["Anthropic-Version"] = true, true
		}
		for name := range request.Header {
			if !allowed[name] {
				t.Fatalf("%s kept client header %s", tt.path, name)
			}
		}
		if request.Header.Get("Authorization") != "Bearer gho_secret" || request.Header.Get("User-Agent") != "opencode/1.18.34" ||
			request.Header.Get("Content-Type") != "application/json" || request.Header.Get("X-Initiator") != "user" {
			t.Fatalf("%s headers = %v", tt.path, request.Header)
		}
		if tt.want == "/v1/messages" && request.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Fatalf("messages without a default Anthropic-Version: %v", request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != tt.body || request.ContentLength != int64(len(tt.body)) {
			t.Fatalf("%s body = %s", tt.path, body)
		}
	}
	models := copilotRequest(t, http.MethodGet, "/v1/models", "", client)
	if models.URL.Path != "/models" || models.Header.Get("X-Initiator") != "" || models.Header.Get("Originator") != "" {
		t.Fatalf("models request = %s %v", models.URL, models.Header)
	}
	for _, rejected := range []struct{ method, path string }{
		{http.MethodPost, "/v1/responses/compact"},
		{http.MethodPost, "/v1/models"},
		{http.MethodGet, "/v1/chat/completions"},
		{http.MethodPost, "/v1/embeddings"},
	} {
		request := httptest.NewRequest(rejected.method, "https://api.githubcopilot.com"+rejected.path, strings.NewReader("{}"))
		if err := providerapi.CopilotRequest(request); err == nil {
			t.Fatalf("accepted %s %s", rejected.method, rejected.path)
		}
	}
}

func TestCopilotRequestMarksAgentTurnsAndVision(t *testing.T) {
	for _, tt := range []struct {
		name, path, body, initiator string
		vision                      bool
	}{
		{"chat prompt", "/v1/chat/completions", `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`, "user", false},
		{"chat follow-up prompt", "/v1/chat/completions", `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":[{"type":"text","text":"c"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "user", true},
		{"chat tool result", "/v1/chat/completions", `{"messages":[{"role":"user","content":"ls"},{"role":"assistant","tool_calls":[]},{"role":"tool","tool_call_id":"c1","content":"a.go"}]}`, "agent", false},
		{"responses string input", "/v1/responses", `{"input":"hi"}`, "user", false},
		{"responses image prompt", "/v1/responses", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:x"}]}]}`, "user", true},
		{"responses function output", "/v1/responses", `{"input":[{"role":"user","content":"ls"},{"type":"function_call","call_id":"c1"},{"type":"function_call_output","call_id":"c1","output":"a.go"}]}`, "agent", false},
		{"messages prompt", "/v1/messages", `{"messages":[{"role":"user","content":"hi"}]}`, "user", false},
		{"messages tool result beside a reminder", "/v1/messages", `{"messages":[{"role":"user","content":"ls"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ls","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{}}]},{"type":"text","text":"<system-reminder>"}]}]}`, "agent", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := copilotRequest(t, http.MethodPost, tt.path, tt.body, nil)
			if got := request.Header.Get("X-Initiator"); got != tt.initiator {
				t.Fatalf("X-Initiator = %q, want %q", got, tt.initiator)
			}
			if got := request.Header.Get("Copilot-Vision-Request") == "true"; got != tt.vision {
				t.Fatalf("Copilot-Vision-Request = %v, want %v", got, tt.vision)
			}
		})
	}
}

func TestCopilotInteractionIDFollowsTheConversationPerAccount(t *testing.T) {
	format := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	id := func(token, body string) string {
		return copilotRequest(t, http.MethodPost, "/v1/messages", body, http.Header{"Authorization": {"Bearer " + token}}).Header.Get("X-Interaction-Id")
	}
	first := id("gho_a", `{"messages":[{"role":"user","content":[{"type":"text","text":"fix it","cache_control":{"type":"ephemeral"}}]}]}`)
	later := id("gho_a", `{"messages":[{"role":"user","content":[{"type":"text","text":"fix it"}]},{"role":"assistant","content":"done"},{"role":"user","content":"thanks"}]}`)
	if !format.MatchString(first) || first != later {
		t.Fatalf("conversation ids = %q, %q", first, later)
	}
	if other := id("gho_b", `{"messages":[{"role":"user","content":[{"type":"text","text":"fix it"}]}]}`); other == first {
		t.Fatal("two accounts share an interaction id")
	}
	if other := id("gho_a", `{"messages":[{"role":"user","content":"another task"}]}`); other == first {
		t.Fatal("two conversations share an interaction id")
	}
}

func TestCopilotMessagesDropOnlyFieldsCopilotRejects(t *testing.T) {
	body := `{"model":"claude-sonnet-4.6","safeguards":{"x":1},` +
		`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","scope":"global"}}],` +
		`"tools":[{"name":"t","eager_input_streaming":true,"input_schema":{"type":"object","properties":{"cache_control":{"type":"object","properties":{"scope":{"type":"string"}}}}},"cache_control":{"type":"ephemeral","scope":"org"}}],` +
		`"messages":[{"role":"user","output_config":{"effort":"high"},"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"r","cache_control":{"type":"ephemeral","scope":"x"}}]},{"type":"text","text":"keep \"scope\" text"}]}]}`
	request := copilotRequest(t, http.MethodPost, "/v1/messages", body, nil)
	raw, _ := io.ReadAll(request.Body)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, gone := range []string{`"safeguards"`, `"output_config"`, `"eager_input_streaming"`, `"scope":"global"`, `"scope":"org"`, `"scope":"x"`} {
		if strings.Contains(text, gone) {
			t.Fatalf("kept %s: %s", gone, text)
		}
	}
	for _, kept := range []string{`"cache_control":{"type":"ephemeral"}`, `"properties":{"scope":{"type":"string"}}`, `keep \"scope\" text`} {
		if !strings.Contains(text, kept) {
			t.Fatalf("lost %s: %s", kept, text)
		}
	}
	if request.ContentLength != int64(len(raw)) {
		t.Fatalf("ContentLength = %d, body %d", request.ContentLength, len(raw))
	}
}

func TestCopilotModelEntryOffersPickerChatModelsOnly(t *testing.T) {
	var catalog struct {
		Data []providerapi.CopilotModelEntry `json:"data"`
	}
	if err := json.Unmarshal([]byte(`{"data":[
		{"id":"claude-sonnet-4.6","model_picker_enabled":true,"supported_endpoints":["/v1/messages","/chat/completions"]},
		{"id":"gpt-5.4","model_picker_enabled":true,"supported_endpoints":["/responses"]},
		{"id":"gpt-4.1","model_picker_enabled":true},
		{"id":"gpt-4o-mini","model_picker_enabled":false,"supported_endpoints":["/chat/completions"]},
		{"id":"text-embedding-3-small","model_picker_enabled":true,"supported_endpoints":["/embeddings"]},
		{"id":"o3","model_picker_enabled":true,"supported_endpoints":["/responses"],"policy":{"state":"disabled"}}
	]}`), &catalog); err != nil {
		t.Fatal(err)
	}
	var offered []string
	for _, entry := range catalog.Data {
		if entry.Offered() {
			offered = append(offered, entry.ID)
		}
	}
	if strings.Join(offered, ",") != "claude-sonnet-4.6,gpt-5.4,gpt-4.1" {
		t.Fatalf("offered = %v", offered)
	}
}
