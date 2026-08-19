package console

import (
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
			{"type":"image_generation","action":"auto"}
		],
		"output":[
			{"type":"custom_tool_call","id":"internal","call_id":"xs_call-1","name":"x_keyword_search","input":"{}"},
			{"type":"function_call","id":"client","call_id":"call_1","name":"x_keyword_search","arguments":"{}"},
			{"type":"image_generation_call","id":"ig_1","status":"completed","result":"aW1hZ2U="},
			{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"done"}]}
		]
	}`))}
	if err := filterConsoleHostedSearchResponse(response, false, route); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, `"id":"internal"`) || strings.Contains(text, `"type":"web_search"`) || strings.Contains(text, `"type":"x_search"`) || strings.Contains(text, `"type":"image_generation","action":"auto"`) {
		t.Fatalf("hosted search trace leaked: %s", text)
	}
	if !strings.Contains(text, `"id":"client"`) || !strings.Contains(text, `"id":"msg_1"`) || !strings.Contains(text, `"type":"function"`) || !strings.Contains(text, `"type":"image_generation_call"`) || !strings.Contains(text, `"result":"aW1hZ2U="`) {
		t.Fatalf("client output was removed: %s", text)
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
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"xs_call-1","name":"x_user_search"}}`, "",
		`event: response.custom_tool_call_input.delta`,
		`data: {"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"ctc_1","delta":"{}"}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"message","id":"msg_1"}}`, "",
		`event: response.image_generation_call.completed`,
		`data: {"type":"response.image_generation_call.completed","output_index":3,"item_id":"ig_1"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"tools":[{"type":"web_search"},{"type":"x_search"},{"type":"image_generation","action":"auto"}],"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"xs_call-1","name":"x_user_search"},{"type":"message","id":"msg_1"},{"type":"image_generation_call","id":"ig_1","status":"completed","result":"aW1hZ2U="}]}}`, "", "",
	}, "\n")
	response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(source))}
	if err := filterConsoleHostedSearchResponse(response, true, route); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "x_user_search") || strings.Contains(text, "ctc_1") || strings.Contains(text, `"type":"x_search"`) || strings.Contains(text, `"type":"web_search"`) || strings.Contains(text, `"type":"image_generation","action":"auto"`) {
		t.Fatalf("hosted search stream trace leaked:\n%s", text)
	}
	if !strings.Contains(text, `"output_index":1`) || !strings.Contains(text, `"id":"msg_1"`) || !strings.Contains(text, `"type":"image_generation_call"`) || !strings.Contains(text, `"result":"aW1hZ2U="`) {
		t.Fatalf("remaining output was not preserved and compacted:\n%s", text)
	}
	if response.ContentLength != -1 || response.Header.Get("Content-Length") != "" {
		t.Fatalf("stream content length = %d header=%q", response.ContentLength, response.Header.Get("Content-Length"))
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
		if err := filterConsoleHostedSearchResponse(response, false, route); err != nil {
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
		if err := filterConsoleHostedSearchResponse(response, true, route); err != nil {
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
