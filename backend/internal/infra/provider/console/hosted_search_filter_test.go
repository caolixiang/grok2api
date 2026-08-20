package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestFilterConsoleHostedSearchResponseJSON(t *testing.T) {
	spec, ok := Resolve("grok-4.5")
	if !ok {
		t.Fatal("grok-4.5 missing")
	}
	_, route, err := normalizeRequestWithRoute([]byte(`{
		"model":"Console/grok-4.5",
		"input":"latest post",
		"tools":[{"type":"function","name":"x_keyword_search","parameters":{"type":"object"}}]
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{
		"id":"resp_1",
		"tools":[
			{"type":"function","name":"x_keyword_search","parameters":{"type":"object"}},
			{"type":"web_search"},
			{"type":"x_search"},
			{"type":"image_generation","action":"auto"},
			{"type":"code_execution"}
		],
		"output":[
			{"type":"custom_tool_call","id":"internal","call_id":"xs_call-1","name":"x_keyword_search","input":"{}"},
			{"type":"function_call","id":"client","call_id":"call_1","name":"x_keyword_search","arguments":"{}"},
			{"type":"image_generation_call","id":"ig_1","status":"completed","result":"aW1hZ2U="},
			{"type":"code_interpreter_call","id":"ci_1","status":"completed","code":"print(2 + 2)","outputs":[{"type":"logs","logs":"{\"stdout\":\"4\",\"output_files\":[{\"file_name\":\"chart.png\",\"mime_type\":\"image/png\",\"data\":[99,104,97,114,116]}]}"}]},
			{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}
		]
	}`))}
	assets := &consoleImageAssetStoreStub{}
	if err := filterConsoleHostedSearchResponse(context.Background(), response, false, route, assets); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, `"id":"internal"`) || strings.Contains(text, `"type":"web_search"`) || strings.Contains(text, `"type":"x_search"`) {
		t.Fatalf("hosted search trace leaked: %s", text)
	}
	if strings.Contains(text, `"type":"image_generation"`) || strings.Contains(text, `"type":"image_generation_call"`) || strings.Contains(text, `"type":"code_execution"`) || strings.Contains(text, `"type":"code_interpreter_call"`) {
		t.Fatalf("server-mounted hosted tool leaked: %s", text)
	}
	if !strings.Contains(text, `"id":"client"`) || !strings.Contains(text, `"id":"msg_1"`) || !strings.Contains(text, `"type":"function"`) ||
		!strings.Contains(text, `![Generated image](https://local.example/v1/media/images/console-1)`) ||
		!strings.Contains(text, `![Generated chart](https://local.example/v1/media/images/console-2)`) {
		t.Fatalf("client output or localized image was removed: %s", text)
	}
	if strings.Contains(text, `msg_grok2api_image_1`) || strings.Contains(text, `msg_grok2api_code_1`) {
		t.Fatalf("localized media was emitted as a separate assistant message: %s", text)
	}
	var filtered struct {
		Output []struct {
			ID      string `json:"id"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Output) != 2 || filtered.Output[1].ID != "msg_1" || len(filtered.Output[1].Content) != 1 ||
		filtered.Output[1].Content[0].Text != "done\n\n![Generated image](https://local.example/v1/media/images/console-1)\n\n![Generated chart](https://local.example/v1/media/images/console-2)" {
		t.Fatalf("localized media was not merged into final assistant text: %s", text)
	}
	if saved := assets.Saved(); len(saved) != 2 || string(saved[0]) != "image" || string(saved[1]) != "chart" {
		t.Fatalf("localized images = %q", saved)
	}
	if response.ContentLength != int64(len(data)) || response.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("content length = %d header=%q want=%d", response.ContentLength, response.Header.Get("Content-Length"), len(data))
	}
}

func TestFilterConsoleHostedSearchResponseStream(t *testing.T) {
	spec, ok := Resolve("grok-4.5")
	if !ok {
		t.Fatal("grok-4.5 missing")
	}
	_, route, err := normalizeRequestWithRoute([]byte(`{"model":"Console/grok-4.5","input":"latest post"}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","tools":[{"type":"web_search"},{"type":"x_search"},{"type":"image_generation","action":"auto"},{"type":"code_interpreter"}],"output":[]}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"xs_call-1","name":"x_user_search"}}`, "",
		`event: response.custom_tool_call_input.delta`,
		`data: {"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"ctc_1","delta":"{}"}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"image_generation_call","id":"ig_1","status":"in_progress","result":null}}`, "",
		`event: response.image_generation_call.in_progress`,
		`data: {"type":"response.image_generation_call.in_progress","output_index":2,"item_id":"ig_1"}`, "",
		`event: response.image_generation_call.generating`,
		`data: {"type":"response.image_generation_call.generating","output_index":2,"item_id":"ig_1"}`, "",
		`event: response.image_generation_call.completed`,
		`data: {"type":"response.image_generation_call.completed","output_index":2,"item_id":"ig_1"}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"image_generation_call","id":"ig_1","status":"completed","result":"aW1hZ2U="}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":3,"item":{"type":"code_interpreter_call","id":"ci_1","status":"in_progress","code":"print(2 + 2)","outputs":[]}}`, "",
		`event: response.code_interpreter_call_code.delta`,
		`data: {"type":"response.code_interpreter_call_code.delta","output_index":3,"item_id":"ci_1","delta":"print(2 + 2)"}`, "",
		`event: response.code_interpreter_call_code.done`,
		`data: {"type":"response.code_interpreter_call_code.done","output_index":3,"item_id":"ci_1","code":"print(2 + 2)"}`, "",
		`event: response.code_interpreter_call.interpreting`,
		`data: {"type":"response.code_interpreter_call.interpreting","output_index":3,"item_id":"ci_1"}`, "",
		`event: response.code_interpreter_call.completed`,
		`data: {"type":"response.code_interpreter_call.completed","output_index":3,"item_id":"ci_1"}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":3,"item":{"type":"code_interpreter_call","id":"ci_1","status":"completed","code":"print(2 + 2)","outputs":[{"type":"logs","logs":"{\"stdout\":\"4\",\"output_files\":[{\"file_name\":\"chart.png\",\"mime_type\":\"image/png\",\"data\":[99,104,97,114,116]}]}"}]}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":4,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`, "",
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","item_id":"msg_1","output_index":4,"content_index":0,"part":{"type":"output_text","text":"","annotations":[],"logprobs":[]}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":4,"content_index":0,"delta":"done","logprobs":[]}`, "",
		`event: response.output_text.done`,
		`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":4,"content_index":0,"text":"done","logprobs":[]}`, "",
		`event: response.content_part.done`,
		`data: {"type":"response.content_part.done","item_id":"msg_1","output_index":4,"content_index":0,"part":{"type":"output_text","text":"done","annotations":[],"logprobs":[]}}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":4,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[],"logprobs":[]}]}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"tools":[{"type":"web_search"},{"type":"x_search"},{"type":"image_generation","action":"auto"},{"type":"code_interpreter"}],"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"xs_call-1","name":"x_user_search"},{"type":"image_generation_call","id":"ig_1","status":"completed","result":"aW1hZ2U="},{"type":"code_interpreter_call","id":"ci_1","status":"completed","code":"print(2 + 2)","outputs":[{"type":"logs","logs":"{\"stdout\":\"4\",\"output_files\":[{\"file_name\":\"chart.png\",\"mime_type\":\"image/png\",\"data\":[99,104,97,114,116]}]}"}]},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[],"logprobs":[]}]}]}}`, "", "",
	}, "\n")
	response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(source))}
	assets := &consoleImageAssetStoreStub{}
	if err := filterConsoleHostedSearchResponse(context.Background(), response, true, route, assets); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "x_user_search") || strings.Contains(text, "ctc_1") || strings.Contains(text, `"type":"x_search"`) || strings.Contains(text, `"type":"web_search"`) {
		t.Fatalf("hosted search stream trace leaked:\n%s", text)
	}
	if strings.Contains(text, `"type":"image_generation"`) || strings.Contains(text, `"type":"response.image_generation_call`) ||
		strings.Contains(text, `"type":"code_execution"`) || strings.Contains(text, `"type":"code_interpreter"`) || strings.Contains(text, `"type":"code_interpreter_call"`) ||
		strings.Contains(text, `"code":"print(2 + 2)"`) ||
		!strings.Contains(text, `"output_index":1`) || !strings.Contains(text, `"id":"msg_1"`) ||
		!strings.Contains(text, `![Generated image](https://local.example/v1/media/images/console-1)`) ||
		!strings.Contains(text, `![Generated chart](https://local.example/v1/media/images/console-2)`) {
		t.Fatalf("server-mounted hosted tools were not rewritten:\n%s", text)
	}
	if strings.Contains(text, `msg_grok2api_image_1`) || strings.Contains(text, `msg_grok2api_code_1`) || strings.Contains(text, `msg_grok2api_media_final`) {
		t.Fatalf("localized media was emitted as a separate assistant message:\n%s", text)
	}
	finalMessageDone := false
	if err := consumeConsoleSSE(strings.NewReader(text), func(event consoleSSEEvent) error {
		var payload struct {
			Type        string `json:"type"`
			OutputIndex int    `json:"output_index"`
			Item        struct {
				ID      string `json:"id"`
				Type    string `json:"type"`
				Status  string `json:"status"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"item"`
		}
		if json.Unmarshal(event.dataBytes(), &payload) == nil && payload.Type == "response.output_item.done" &&
			payload.OutputIndex == 1 && payload.Item.ID == "msg_1" && payload.Item.Type == "message" && payload.Item.Status == "completed" &&
			len(payload.Item.Content) == 1 && payload.Item.Content[0].Text == "done\n\n![Generated image](https://local.example/v1/media/images/console-1)\n\n![Generated chart](https://local.example/v1/media/images/console-2)" {
			finalMessageDone = true
		}
		return nil
	}); err != nil || !finalMessageDone {
		t.Fatalf("localized media was not merged into final assistant terminal event (err=%v done=%t):\n%s", err, finalMessageDone, text)
	}
	if saved := assets.Saved(); len(saved) != 2 || string(saved[0]) != "image" || string(saved[1]) != "chart" {
		t.Fatalf("localized stream images = %q", saved)
	}
	if response.ContentLength != -1 || response.Header.Get("Content-Length") != "" {
		t.Fatalf("stream content length = %d header=%q", response.ContentLength, response.Header.Get("Content-Length"))
	}
}

func TestFilterConsoleHostedSearchResponseStreamFlushesTerminalMedia(t *testing.T) {
	spec, ok := Resolve("grok-4.5")
	if !ok {
		t.Fatal("grok-4.5 missing")
	}
	_, route, err := normalizeRequestWithRoute([]byte(`{"model":"Console/grok-4.5","input":"draw"}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress","tools":[{"type":"image_generation","action":"auto"}],"output":[]}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"image_generation_call","id":"ig_1","status":"in_progress","result":null}}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"image_generation_call","id":"ig_1","status":"completed","result":"aW1hZ2U="}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","sequence_number":3,"response":{"id":"resp_1","status":"completed","tools":[{"type":"image_generation","action":"auto"}],"output":[{"type":"image_generation_call","id":"ig_1","status":"completed","result":"aW1hZ2U="}]}}`, "", "",
	}, "\n")
	response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(source))}
	assets := &consoleImageAssetStoreStub{}
	if err := filterConsoleHostedSearchResponse(context.Background(), response, true, route, assets); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, `"type":"image_generation_call"`) || !strings.Contains(text, `"id":"msg_grok2api_media_final"`) ||
		!strings.Contains(text, `![Generated image](https://local.example/v1/media/images/console-1)`) {
		t.Fatalf("terminal media was not emitted as the final assistant message:\n%s", text)
	}
	sequences := make([]int, 0, 8)
	eventTypes := make([]string, 0, 8)
	if err := consumeConsoleSSE(strings.NewReader(text), func(event consoleSSEEvent) error {
		var payload struct {
			Type           string `json:"type"`
			SequenceNumber int    `json:"sequence_number"`
		}
		if err := json.Unmarshal(event.dataBytes(), &payload); err != nil {
			return err
		}
		sequences = append(sequences, payload.SequenceNumber)
		eventTypes = append(eventTypes, payload.Type)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sequences) != 8 {
		t.Fatalf("terminal media events = %d, want 8:\n%s", len(sequences), text)
	}
	for index, sequence := range sequences {
		if sequence != index {
			t.Fatalf("sequence[%d] = %d, want %d:\n%s", index, sequence, index, text)
		}
	}
	if eventTypes[len(eventTypes)-2] != "response.output_item.done" || eventTypes[len(eventTypes)-1] != "response.completed" {
		t.Fatalf("terminal event order = %v", eventTypes)
	}
}

func TestFilterConsoleHostedSearchResponsePreservesClientDeclaredHostedTools(t *testing.T) {
	spec, ok := Resolve("grok-4.5")
	if !ok {
		t.Fatal("grok-4.5 missing")
	}
	_, route, err := normalizeRequestWithRoute([]byte(`{
		"model":"Console/grok-4.5",
		"input":"draw and calculate",
		"tools":[{"type":"image_generation"},{"type":"code_execution"}]
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, injected := route.injectedToolTypes["image_generation"]; injected {
		t.Fatal("client-declared image_generation marked as injected")
	}
	if _, injected := route.injectedToolTypes["code_execution"]; injected {
		t.Fatal("client-declared code_execution marked as injected")
	}

	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","sequence_number":0,"response":{"tools":[{"type":"web_search"},{"type":"x_search"},{"type":"image_generation"},{"type":"code_execution"}],"output":[]}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"image_generation_call","id":"ig_client","status":"in_progress"}}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"image_generation_call","id":"ig_client","status":"completed","result":"aW1hZ2U="}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"type":"code_interpreter_call","id":"ci_client","status":"in_progress"}}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","sequence_number":4,"output_index":1,"item":{"type":"code_interpreter_call","id":"ci_client","status":"completed","outputs":[{"type":"logs","logs":"4"}]}}`, "", "",
	}, "\n")
	response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(source))}
	assets := &consoleImageAssetStoreStub{}
	if err := filterConsoleHostedSearchResponse(context.Background(), response, true, route, assets); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{`"type":"image_generation"`, `"type":"image_generation_call"`, `"id":"ig_client"`, `"type":"code_execution"`, `"type":"code_interpreter_call"`, `"id":"ci_client"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("client-declared hosted tool lifecycle was removed (%s):\n%s", required, text)
		}
	}
	if saved := assets.Saved(); len(saved) != 0 {
		t.Fatalf("client-declared image was unexpectedly localized: %q", saved)
	}
}

func TestFilterConsoleHostedSearchResponseRestoresAliasedViewImage(t *testing.T) {
	spec, ok := Resolve("grok-4.5")
	if !ok {
		t.Fatal("grok-4.5 missing")
	}
	_, route, err := normalizeRequestWithRoute([]byte(`{
		"model":"Console/grok-4.5",
		"input":"inspect an image",
		"tools":[{"type":"function","name":"view_image","parameters":{"type":"object"}}]
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !route.aliasedViewImage {
		t.Fatal("view_image alias route was not recorded")
	}

	t.Run("json", func(t *testing.T) {
		response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{
			"id":"resp_1",
			"tools":[{"type":"function","name":"view_image_local_grok2api","parameters":{"type":"object"}}],
			"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"view_image_local_grok2api","arguments":"{}"}]
		}`))}
		if err := filterConsoleHostedSearchResponse(context.Background(), response, false, route, nil); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, consoleViewImageToolAlias) || strings.Count(text, `"name":"view_image"`) != 2 {
			t.Fatalf("view_image alias was not restored: %s", text)
		}
	})

	t.Run("stream", func(t *testing.T) {
		source := strings.Join([]string{
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"view_image_local_grok2api","arguments":"{}"}}`, "",
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"view_image_local_grok2api","arguments":"{}"}]}}`, "", "",
		}, "\n")
		response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(source))}
		if err := filterConsoleHostedSearchResponse(context.Background(), response, true, route, nil); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, consoleViewImageToolAlias) || strings.Count(text, `"name":"view_image"`) != 2 {
			t.Fatalf("stream view_image alias was not restored:\n%s", text)
		}
	})
}
