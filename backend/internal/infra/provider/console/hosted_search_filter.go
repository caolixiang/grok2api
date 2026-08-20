package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	maxConsoleSearchResponseBytes = 64 << 20
	maxConsoleSearchSSEEventBytes = 8 << 20
)

// filterConsoleHostedSearchResponse hides xAI's completed native-search
// subcalls. They are execution traces, not client tools; forwarding them makes
// Responses clients try to execute x_user_search/x_keyword_search a second time.
// Client-declared hosted tools retain their native lifecycle. Tools mounted by
// the server are provider internals: exposing them makes Responses SDKs reject
// the call because they were not present in the client's tool registry.
func filterConsoleHostedSearchResponse(ctx context.Context, response *http.Response, streaming bool, route consoleHostedToolRoute, assets provider.ImageAssetStore) error {
	if response == nil || response.Body == nil || (!route.hasXSearch && len(route.injectedToolTypes) == 0) {
		return nil
	}
	if !streaming && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return nil
	}
	filter := newConsoleHostedSearchFilter(route, assets)
	if streaming {
		response.Body = filter.stream(ctx, response.Body)
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
	filtered, err := filter.filterJSON(ctx, data)
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
	assets               provider.ImageAssetStore
	droppedOutputIndexes map[int]struct{}
	droppedItemIDs       map[string]struct{}
	localizedImages      map[string]consoleLocalizedImage
	localizedCodeCalls   map[string]consoleLocalizedImage
	sequenceAdjustment   int
}

type consoleLocalizedImage struct {
	messageID string
	text      string
}

type consoleHostedSearchStream struct {
	io.ReadCloser
	source io.ReadCloser
}

func newConsoleHostedSearchFilter(route consoleHostedToolRoute, assets provider.ImageAssetStore) *consoleHostedSearchFilter {
	return &consoleHostedSearchFilter{
		route:                route,
		assets:               assets,
		droppedOutputIndexes: make(map[int]struct{}),
		droppedItemIDs:       make(map[string]struct{}),
		localizedImages:      make(map[string]consoleLocalizedImage),
		localizedCodeCalls:   make(map[string]consoleLocalizedImage),
	}
}

func (f *consoleHostedSearchFilter) stream(ctx context.Context, source io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		err := consumeConsoleSSE(source, func(event consoleSSEEvent) error {
			if !event.hasData() || bytes.Equal(bytes.TrimSpace(event.dataBytes()), []byte("[DONE]")) {
				return event.writeTo(writer)
			}
			filtered, filterErr := f.filterStreamEvent(ctx, event)
			if filterErr != nil {
				return filterErr
			}
			for _, output := range filtered {
				if err := output.writeTo(writer); err != nil {
					return err
				}
			}
			return nil
		})
		_ = writer.CloseWithError(err)
	}()
	return &consoleHostedSearchStream{ReadCloser: reader, source: source}
}

func (f *consoleHostedSearchFilter) filterJSON(ctx context.Context, body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Console Responses 响应: %w", err)
	}
	if err := f.filterEnvelope(ctx, payload); err != nil {
		return nil, err
	}
	f.restoreReservedFunctionNames(payload)
	return json.Marshal(payload)
}

func (f *consoleHostedSearchFilter) filterStreamEvent(ctx context.Context, event consoleSSEEvent) ([]consoleSSEEvent, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.dataBytes(), &payload); err != nil {
		return []consoleSSEEvent{event}, nil
	}
	sequence, hasSequence := consoleJSONInt(payload["sequence_number"])
	var eventType string
	_ = json.Unmarshal(payload["type"], &eventType)
	item := payload["item"]

	if f.isInjectedImageEvent(eventType, item) {
		if eventType == "response.output_item.done" && f.isInjectedImageCall(item) {
			localized, err := f.localizeImageCall(ctx, item)
			if err != nil {
				return nil, err
			}
			outputIndex, _ := consoleJSONInt(payload["output_index"])
			outputIndex = f.compactedOutputIndex(outputIndex)
			events := consoleImageMessageEvents(localized, outputIndex)
			f.numberReplacementEvents(events, sequence, hasSequence)
			return events, nil
		}
		f.recordRemovedSequence(hasSequence)
		return nil, nil
	}

	if f.isInjectedCodeEvent(eventType, item) {
		if eventType == "response.output_item.done" && f.isInjectedCodeCall(item) {
			localized, found, err := f.localizeCodeCall(ctx, item)
			if err != nil {
				return nil, err
			}
			if found {
				outputIndex, _ := consoleJSONInt(payload["output_index"])
				outputIndex = f.compactedOutputIndex(outputIndex)
				events := consoleImageMessageEvents(localized, outputIndex)
				f.numberReplacementEvents(events, sequence, hasSequence)
				return events, nil
			}
			f.recordDroppedItem(payload, item)
		}
		f.recordRemovedSequence(hasSequence)
		return nil, nil
	}

	if !emptyConsoleJSON(item) && f.isInternalCall(item) {
		f.recordDroppedItem(payload, item)
		f.recordRemovedSequence(hasSequence)
		return nil, nil
	}
	if err := f.filterEnvelope(ctx, payload); err != nil {
		return nil, err
	}
	if f.referencesDroppedItem(payload) {
		f.recordRemovedSequence(hasSequence)
		return nil, nil
	}
	f.compactOutputIndex(payload)
	f.restoreReservedFunctionNames(payload)
	if hasSequence {
		payload["sequence_number"] = consoleJSON(sequence + f.sequenceAdjustment)
	}
	filtered, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	event.setData(filtered)
	return []consoleSSEEvent{event}, nil
}

func (f *consoleHostedSearchFilter) isInjectedImageEvent(eventType string, item json.RawMessage) bool {
	if _, injected := f.route.injectedToolTypes["image_generation"]; !injected {
		return false
	}
	return strings.HasPrefix(eventType, "response.image_generation_call.") || f.isInjectedImageCall(item)
}

func (f *consoleHostedSearchFilter) recordRemovedSequence(hasSequence bool) {
	if hasSequence {
		f.sequenceAdjustment--
	}
}

func (f *consoleHostedSearchFilter) numberReplacementEvents(events []consoleSSEEvent, sequence int, hasSequence bool) {
	if !hasSequence {
		return
	}
	start := sequence + f.sequenceAdjustment
	for index := range events {
		var payload map[string]json.RawMessage
		if json.Unmarshal(events[index].dataBytes(), &payload) != nil {
			continue
		}
		payload["sequence_number"] = consoleJSON(start + index)
		encoded, _ := json.Marshal(payload)
		events[index].setData(encoded)
	}
	f.sequenceAdjustment += len(events) - 1
}

func (f *consoleHostedSearchFilter) filterEnvelope(ctx context.Context, payload map[string]json.RawMessage) error {
	if err := f.filterOutput(ctx, payload); err != nil {
		return err
	}
	if err := f.filterTools(payload); err != nil {
		return err
	}
	if raw := payload["response"]; !emptyConsoleJSON(raw) {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil && response != nil {
			if err := f.filterOutput(ctx, response); err != nil {
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

func (f *consoleHostedSearchFilter) filterOutput(ctx context.Context, envelope map[string]json.RawMessage) error {
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
		if f.isInjectedImageCall(item) {
			localized, err := f.localizeImageCall(ctx, item)
			if err != nil {
				return err
			}
			filtered = append(filtered, consoleJSON(consoleImageMessageItem(localized)))
			continue
		}
		if f.isInjectedCodeCall(item) {
			localized, found, err := f.localizeCodeCall(ctx, item)
			if err != nil {
				return err
			}
			if found {
				filtered = append(filtered, consoleJSON(consoleImageMessageItem(localized)))
			}
			continue
		}
		if f.isInternalCall(item) {
			continue
		}
		filtered = append(filtered, item)
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
		injectedKind := kind
		if kind == "code_interpreter" {
			injectedKind = "code_execution"
		}
		_, injected := f.route.injectedToolTypes[injectedKind]
		if !injected {
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
	if kind == "code_interpreter_call" {
		_, injected := f.route.injectedToolTypes["code_execution"]
		return injected
	}
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

func (f *consoleHostedSearchFilter) isInjectedImageCall(raw json.RawMessage) bool {
	if _, injected := f.route.injectedToolTypes["image_generation"]; !injected {
		return false
	}
	var item consoleSearchCall
	return json.Unmarshal(raw, &item) == nil && item.Type == "image_generation_call"
}

func (f *consoleHostedSearchFilter) isInjectedCodeEvent(eventType string, item json.RawMessage) bool {
	if _, injected := f.route.injectedToolTypes["code_execution"]; !injected {
		return false
	}
	return strings.HasPrefix(eventType, "response.code_interpreter_call") || f.isInjectedCodeCall(item)
}

func (f *consoleHostedSearchFilter) isInjectedCodeCall(raw json.RawMessage) bool {
	if _, injected := f.route.injectedToolTypes["code_execution"]; !injected {
		return false
	}
	var item consoleSearchCall
	return json.Unmarshal(raw, &item) == nil && item.Type == "code_interpreter_call"
}

func (f *consoleHostedSearchFilter) localizeImageCall(ctx context.Context, raw json.RawMessage) (consoleLocalizedImage, error) {
	var item struct {
		ID     string `json:"id"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return consoleLocalizedImage{}, fmt.Errorf("解析 Console 图片工具结果: %w", err)
	}
	item.ID = strings.TrimSpace(item.ID)
	if localized, exists := f.localizedImages[item.ID]; exists {
		return localized, nil
	}
	if f.assets == nil {
		return consoleLocalizedImage{}, fmt.Errorf("Console Responses 自动图片工具需要媒体存储")
	}
	encoded := strings.TrimSpace(item.Result)
	if prefix, value, found := strings.Cut(encoded, ","); found && strings.HasPrefix(strings.ToLower(prefix), "data:image/") {
		encoded = value
	}
	image, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(image) == 0 {
		return consoleLocalizedImage{}, fmt.Errorf("解析 Console 图片工具 Base64 结果失败")
	}
	asset, err := f.assets.SaveImage(ctx, image)
	if err != nil {
		return consoleLocalizedImage{}, fmt.Errorf("保存 Console Responses 图片工具结果: %w", err)
	}
	localized := consoleLocalizedImage{
		messageID: consoleImageMessageID(item.ID),
		text:      fmt.Sprintf("![Generated image](%s)", f.assets.PublicImageURL(asset.ID)),
	}
	f.localizedImages[item.ID] = localized
	return localized, nil
}

func consoleImageMessageID(toolID string) string {
	toolID = strings.TrimSpace(toolID)
	if suffix, found := strings.CutPrefix(toolID, "ig_"); found {
		return "msg_grok2api_image_" + suffix
	}
	return "msg_grok2api_image_" + toolID
}

type consoleCodeInterpreterCall struct {
	ID      string `json:"id"`
	Outputs []struct {
		Type string          `json:"type"`
		Logs json.RawMessage `json:"logs"`
	} `json:"outputs"`
}

type consoleCodeExecutionLog struct {
	OutputFiles []struct {
		FileName string          `json:"file_name"`
		MIMEType string          `json:"mime_type"`
		Data     json.RawMessage `json:"data"`
	} `json:"output_files"`
}

func (f *consoleHostedSearchFilter) localizeCodeCall(ctx context.Context, raw json.RawMessage) (consoleLocalizedImage, bool, error) {
	var item consoleCodeInterpreterCall
	if err := json.Unmarshal(raw, &item); err != nil {
		return consoleLocalizedImage{}, false, fmt.Errorf("解析 Console 代码工具结果: %w", err)
	}
	item.ID = strings.TrimSpace(item.ID)
	if localized, exists := f.localizedCodeCalls[item.ID]; exists {
		return localized, true, nil
	}

	images := make([][]byte, 0)
	for _, output := range item.Outputs {
		if output.Type != "logs" || emptyConsoleJSON(output.Logs) {
			continue
		}
		logs := output.Logs
		var encodedLogs string
		if json.Unmarshal(output.Logs, &encodedLogs) == nil {
			logs = json.RawMessage(encodedLogs)
		}
		var log consoleCodeExecutionLog
		if json.Unmarshal(logs, &log) != nil {
			continue
		}
		for _, file := range log.OutputFiles {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(file.MIMEType)), "image/") {
				continue
			}
			image, err := decodeConsoleCodeOutputFile(file.Data)
			if err != nil {
				return consoleLocalizedImage{}, false, fmt.Errorf("解析 Console 代码工具图片 %q: %w", file.FileName, err)
			}
			images = append(images, image)
		}
	}
	if len(images) == 0 {
		return consoleLocalizedImage{}, false, nil
	}
	if f.assets == nil {
		return consoleLocalizedImage{}, false, fmt.Errorf("Console Responses 自动代码工具需要媒体存储")
	}
	links := make([]string, 0, len(images))
	for _, image := range images {
		asset, err := f.assets.SaveImage(ctx, image)
		if err != nil {
			return consoleLocalizedImage{}, false, fmt.Errorf("保存 Console Responses 代码工具图片: %w", err)
		}
		links = append(links, fmt.Sprintf("![Generated chart](%s)", f.assets.PublicImageURL(asset.ID)))
	}
	localized := consoleLocalizedImage{
		messageID: consoleCodeMessageID(item.ID),
		text:      strings.Join(links, "\n\n"),
	}
	f.localizedCodeCalls[item.ID] = localized
	return localized, true, nil
}

func decodeConsoleCodeOutputFile(raw json.RawMessage) ([]byte, error) {
	if emptyConsoleJSON(raw) {
		return nil, fmt.Errorf("图片数据为空")
	}
	var bytesValue []byte
	if err := json.Unmarshal(raw, &bytesValue); err == nil && len(bytesValue) > 0 {
		return bytesValue, nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, fmt.Errorf("图片数据格式不受支持")
	}
	encoded = strings.TrimSpace(encoded)
	if prefix, value, found := strings.Cut(encoded, ","); found && strings.HasPrefix(strings.ToLower(prefix), "data:image/") {
		encoded = value
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return nil, fmt.Errorf("图片 Base64 数据无效")
	}
	return decoded, nil
}

func consoleCodeMessageID(toolID string) string {
	toolID = strings.TrimSpace(toolID)
	if suffix, found := strings.CutPrefix(toolID, "ci_"); found {
		return "msg_grok2api_code_" + suffix
	}
	return "msg_grok2api_code_" + toolID
}

func consoleImageMessageItem(image consoleLocalizedImage) map[string]any {
	return map[string]any{
		"id":     image.messageID,
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        image.text,
			"annotations": []any{},
			"logprobs":    []any{},
		}},
	}
}

func consoleImageMessageEvents(image consoleLocalizedImage, outputIndex int) []consoleSSEEvent {
	emptyPart := map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}
	completedPart := map[string]any{"type": "output_text", "text": image.text, "annotations": []any{}, "logprobs": []any{}}
	completedItem := consoleImageMessageItem(image)
	payloads := []map[string]any{
		{
			"type": "response.output_item.added", "output_index": outputIndex,
			"item": map[string]any{"id": image.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
		},
		{
			"type": "response.content_part.added", "item_id": image.messageID,
			"output_index": outputIndex, "content_index": 0, "part": emptyPart,
		},
		{
			"type": "response.output_text.delta", "item_id": image.messageID,
			"output_index": outputIndex, "content_index": 0, "delta": image.text, "logprobs": []any{},
		},
		{
			"type": "response.output_text.done", "item_id": image.messageID,
			"output_index": outputIndex, "content_index": 0, "text": image.text, "logprobs": []any{},
		},
		{
			"type": "response.content_part.done", "item_id": image.messageID,
			"output_index": outputIndex, "content_index": 0, "part": completedPart,
		},
		{"type": "response.output_item.done", "output_index": outputIndex, "item": completedItem},
	}
	events := make([]consoleSSEEvent, 0, len(payloads))
	for _, payload := range payloads {
		typeName, _ := payload["type"].(string)
		encoded, _ := json.Marshal(payload)
		events = append(events, consoleSSEEvent{event: typeName, data: []string{string(encoded)}})
	}
	return events
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
	payload["output_index"] = consoleJSON(f.compactedOutputIndex(index))
}

func (f *consoleHostedSearchFilter) compactedOutputIndex(index int) int {
	removedBefore := 0
	for dropped := range f.droppedOutputIndexes {
		if dropped < index {
			removedBefore++
		}
	}
	return index - removedBefore
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
