package console

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	maxConsoleSearchResponseBytes = 64 << 20
	maxConsoleSearchSSEEventBytes = 8 << 20
)

// filterConsoleHostedSearchResponse hides xAI's completed native-search
// subcalls. They are execution traces, not client tools; forwarding them makes
// Responses clients try to execute x_user_search/x_keyword_search a second time.
// Image generation stays visible because clients need its tool definition to
// correlate the streamed image_generation_call lifecycle.
func filterConsoleHostedSearchResponse(response *http.Response, streaming bool, route consoleHostedToolRoute) error {
	if response == nil || response.Body == nil || (!route.hasXSearch && len(route.injectedToolTypes) == 0) {
		return nil
	}
	if !streaming && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return nil
	}
	filter := newConsoleHostedSearchFilter(route)
	if streaming {
		response.Body = filter.stream(response.Body)
		response.Header.Del("Content-Length")
		response.ContentLength = -1
		return nil
	}
	source := response.Body
	data, err := io.ReadAll(io.LimitReader(source, maxConsoleSearchResponseBytes+1))
	_ = source.Close()
	if err != nil {
		return err
	}
	if len(data) > maxConsoleSearchResponseBytes {
		return fmt.Errorf("Console Responses 响应超过 %d MiB", maxConsoleSearchResponseBytes>>20)
	}
	filtered, err := filter.filterJSON(data)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(filtered))
	response.Header.Set("Content-Length", strconv.Itoa(len(filtered)))
	response.ContentLength = int64(len(filtered))
	return nil
}

type consoleHostedSearchFilter struct {
	route                consoleHostedToolRoute
	droppedOutputIndexes map[int]struct{}
	droppedItemIDs       map[string]struct{}
}

type consoleHostedSearchStream struct {
	io.ReadCloser
	source io.ReadCloser
}

func newConsoleHostedSearchFilter(route consoleHostedToolRoute) *consoleHostedSearchFilter {
	return &consoleHostedSearchFilter{
		route:                route,
		droppedOutputIndexes: make(map[int]struct{}),
		droppedItemIDs:       make(map[string]struct{}),
	}
}

func (f *consoleHostedSearchFilter) stream(source io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		err := consumeConsoleSSE(source, func(event consoleSSEEvent) error {
			if !event.hasData() || bytes.Equal(bytes.TrimSpace(event.dataBytes()), []byte("[DONE]")) {
				return event.writeTo(writer)
			}
			filtered, keep, filterErr := f.filterEvent(event.dataBytes())
			if filterErr != nil {
				return filterErr
			}
			if !keep {
				return nil
			}
			event.setData(filtered)
			return event.writeTo(writer)
		})
		_ = writer.CloseWithError(err)
	}()
	return &consoleHostedSearchStream{ReadCloser: reader, source: source}
}

func (f *consoleHostedSearchFilter) filterJSON(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Console Responses 响应: %w", err)
	}
	if err := f.filterEnvelope(payload); err != nil {
		return nil, err
	}
	f.restoreReservedFunctionNames(payload)
	return json.Marshal(payload)
}

func (f *consoleHostedSearchFilter) filterEvent(data []byte) ([]byte, bool, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return data, true, nil
	}
	if item := payload["item"]; !emptyConsoleJSON(item) && f.isInternalCall(item) {
		f.recordDroppedItem(payload, item)
		return nil, false, nil
	}
	if err := f.filterEnvelope(payload); err != nil {
		return nil, false, err
	}
	if f.referencesDroppedItem(payload) {
		return nil, false, nil
	}
	f.compactOutputIndex(payload)
	f.restoreReservedFunctionNames(payload)
	filtered, err := json.Marshal(payload)
	return filtered, true, err
}

func (f *consoleHostedSearchFilter) filterEnvelope(payload map[string]json.RawMessage) error {
	if err := f.filterOutput(payload); err != nil {
		return err
	}
	if err := f.filterTools(payload); err != nil {
		return err
	}
	if raw := payload["response"]; !emptyConsoleJSON(raw) {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil && response != nil {
			if err := f.filterOutput(response); err != nil {
				return err
			}
			if err := f.filterTools(response); err != nil {
				return err
			}
			payload["response"] = consoleJSON(response)
		}
	}
	return nil
}

func (f *consoleHostedSearchFilter) filterOutput(envelope map[string]json.RawMessage) error {
	raw := envelope["output"]
	if emptyConsoleJSON(raw) {
		return nil
	}
	var output []json.RawMessage
	if json.Unmarshal(raw, &output) != nil {
		return fmt.Errorf("解析 Console Responses output 失败")
	}
	filtered := make([]json.RawMessage, 0, len(output))
	for _, item := range output {
		if !f.isInternalCall(item) {
			filtered = append(filtered, item)
		}
	}
	envelope["output"] = consoleJSON(filtered)
	return nil
}

func (f *consoleHostedSearchFilter) filterTools(envelope map[string]json.RawMessage) error {
	if len(f.route.injectedToolTypes) == 0 || emptyConsoleJSON(envelope["tools"]) {
		return nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(envelope["tools"], &tools) != nil {
		return fmt.Errorf("解析 Console Responses tools 失败")
	}
	filtered := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		var value struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(tool, &value)
		kind := strings.ToLower(strings.TrimSpace(value.Type))
		_, injected := f.route.injectedToolTypes[kind]
		if !injected || kind == "image_generation" {
			filtered = append(filtered, tool)
		}
	}
	if len(filtered) == 0 {
		delete(envelope, "tools")
		return nil
	}
	envelope["tools"] = consoleJSON(filtered)
	return nil
}

type consoleSearchCall struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func (f *consoleHostedSearchFilter) isInternalCall(raw json.RawMessage) bool {
	var item consoleSearchCall
	if json.Unmarshal(raw, &item) != nil {
		return false
	}
	kind := strings.TrimSpace(item.Type)
	if kind == "web_search_call" {
		_, injected := f.route.injectedToolTypes["web_search"]
		return injected
	}
	if kind != "custom_tool_call" && kind != "function_call" {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(item.CallID), "xs_call") {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(item.Name))
	if !isConsoleInternalXSearchName(name) || strings.TrimSpace(item.Namespace) != "" {
		return false
	}
	_, declared := f.route.clientDeclaredTools[name]
	return !declared
}

func isConsoleInternalXSearchName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "x_user_search", "x_semantic_search", "x_keyword_search", "x_thread_fetch":
		return true
	default:
		return false
	}
}

func (f *consoleHostedSearchFilter) restoreReservedFunctionNames(payload map[string]json.RawMessage) {
	if f == nil || !f.route.aliasedViewImage {
		return
	}
	for key, raw := range payload {
		if emptyConsoleJSON(raw) {
			continue
		}
		var value any
		if json.Unmarshal(raw, &value) != nil || !restoreConsoleViewImageName(value) {
			continue
		}
		payload[key] = consoleJSON(value)
	}
}

func restoreConsoleViewImageName(value any) bool {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		if name, ok := typed["name"].(string); ok && name == consoleViewImageToolAlias {
			typed["name"] = consoleViewImageToolName
			changed = true
		}
		for _, child := range typed {
			if restoreConsoleViewImageName(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range typed {
			if restoreConsoleViewImageName(child) {
				changed = true
			}
		}
	}
	return changed
}

func (f *consoleHostedSearchFilter) recordDroppedItem(payload map[string]json.RawMessage, rawItem json.RawMessage) {
	if index, ok := consoleJSONInt(payload["output_index"]); ok {
		f.droppedOutputIndexes[index] = struct{}{}
	}
	var item consoleSearchCall
	if json.Unmarshal(rawItem, &item) != nil {
		return
	}
	for _, value := range []string{item.ID, item.CallID} {
		if value = strings.TrimSpace(value); value != "" {
			f.droppedItemIDs[value] = struct{}{}
		}
	}
}

func (f *consoleHostedSearchFilter) referencesDroppedItem(payload map[string]json.RawMessage) bool {
	if index, ok := consoleJSONInt(payload["output_index"]); ok {
		if _, dropped := f.droppedOutputIndexes[index]; dropped {
			return true
		}
	}
	for _, field := range []string{"item_id", "call_id"} {
		var value string
		_ = json.Unmarshal(payload[field], &value)
		if _, dropped := f.droppedItemIDs[strings.TrimSpace(value)]; dropped && value != "" {
			return true
		}
	}
	return false
}

func (f *consoleHostedSearchFilter) compactOutputIndex(payload map[string]json.RawMessage) {
	index, ok := consoleJSONInt(payload["output_index"])
	if !ok {
		return
	}
	removedBefore := 0
	for dropped := range f.droppedOutputIndexes {
		if dropped < index {
			removedBefore++
		}
	}
	if removedBefore > 0 {
		payload["output_index"] = consoleJSON(index - removedBefore)
	}
}

func emptyConsoleJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func consoleJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func consoleJSONInt(raw json.RawMessage) (int, bool) {
	if emptyConsoleJSON(raw) {
		return 0, false
	}
	value, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	return value, err == nil
}

type consoleSSEEvent struct {
	event    string
	id       string
	retry    string
	comments []string
	other    []string
	data     []string
}

func (e consoleSSEEvent) dataBytes() []byte { return []byte(strings.Join(e.data, "\n")) }
func (e consoleSSEEvent) hasData() bool     { return len(e.data) > 0 }
func (e *consoleSSEEvent) setData(data []byte) {
	e.data = strings.Split(string(data), "\n")
}

func (e consoleSSEEvent) writeTo(writer io.Writer) error {
	for _, line := range e.comments {
		if _, err := fmt.Fprintln(writer, line); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{{"event", e.event}, {"id", e.id}, {"retry", e.retry}} {
		if field.value != "" {
			if _, err := fmt.Fprintf(writer, "%s: %s\n", field.name, field.value); err != nil {
				return err
			}
		}
	}
	for _, line := range e.other {
		if _, err := fmt.Fprintln(writer, line); err != nil {
			return err
		}
	}
	for _, line := range e.data {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer)
	return err
}

func consumeConsoleSSE(source io.Reader, handle func(consoleSSEEvent) error) error {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64<<10), maxConsoleSearchSSEEventBytes)
	event := consoleSSEEvent{}
	eventBytes := 0
	flush := func() error {
		if !event.hasData() && len(event.comments) == 0 && len(event.other) == 0 && event.event == "" && event.id == "" && event.retry == "" {
			return nil
		}
		current := event
		event = consoleSSEEvent{}
		eventBytes = 0
		return handle(current)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		eventBytes += len(line)
		if eventBytes > maxConsoleSearchSSEEventBytes {
			return fmt.Errorf("Console Responses SSE 单事件超过 %d MiB", maxConsoleSearchSSEEventBytes>>20)
		}
		field, value, found := strings.Cut(line, ":")
		if found && strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch {
		case strings.HasPrefix(line, ":"):
			event.comments = append(event.comments, line)
		case !found:
			event.other = append(event.other, line)
		case field == "event":
			event.event = value
		case field == "data":
			event.data = append(event.data, value)
		case field == "id":
			event.id = value
		case field == "retry":
			event.retry = value
		default:
			event.other = append(event.other, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
