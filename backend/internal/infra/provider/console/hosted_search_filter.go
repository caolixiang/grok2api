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
	pendingMedia         []consoleLocalizedImage
	mediaTargets         map[string]string
	messageMedia         map[string][]string
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
		mediaTargets:         make(map[string]string),
		messageMedia:         make(map[string][]string),
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
	terminalMedia, err := f.prepareTerminalMedia(eventType, payload)
	if err != nil {
		return nil, err
	}

	if f.isInjectedImageEvent(eventType, item) {
		if eventType == "response.output_item.done" && f.isInjectedImageCall(item) {
			localized, err := f.localizeImageCall(ctx, item)
			if err != nil {
				return nil, err
			}
			f.pendingMedia = append(f.pendingMedia, localized)
		}
		f.recordDroppedItem(payload, item)
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
	f.appendMediaToTerminalPayload(eventType, payload)
	f.restoreReservedFunctionNames(payload)
	if terminalMedia != nil {
		filtered, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		event.setData(filtered)
		outputIndex := consoleMessageOutputIndex(payload, terminalMedia.messageID)
		events := append(consoleImageMessageEvents(*terminalMedia, outputIndex), event)
		f.numberReplacementEvents(events, sequence, hasSequence)
		return events, nil
	}
	mediaDelta := f.attachPendingMediaToTextDone(eventType, payload)
	if mediaDelta != nil {
		filtered, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		event.setData(filtered)
		deltaType, _ := mediaDelta["type"].(string)
		deltaData, _ := json.Marshal(mediaDelta)
		events := []consoleSSEEvent{{event: deltaType, data: []string{string(deltaData)}}, event}
		f.numberReplacementEvents(events, sequence, hasSequence)
		return events, nil
	}
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

func (f *consoleHostedSearchFilter) prepareTerminalMedia(eventType string, payload map[string]json.RawMessage) (*consoleLocalizedImage, error) {
	if eventType != "response.completed" || len(f.pendingMedia) == 0 {
		return nil, nil
	}
	media := make([]string, 0, len(f.pendingMedia))
	for _, localized := range f.pendingMedia {
		media = append(media, localized.text)
	}
	terminal := consoleLocalizedImage{
		messageID: "msg_grok2api_media_final",
		text:      strings.Join(media, "\n\n"),
	}
	for _, localized := range f.pendingMedia {
		f.mediaTargets[localized.messageID] = terminal.messageID
	}
	f.pendingMedia = nil

	target := payload
	if raw := payload["response"]; !emptyConsoleJSON(raw) {
		var response map[string]json.RawMessage
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, fmt.Errorf("解析 Console Responses 完成事件: %w", err)
		}
		target = response
		defer func() { payload["response"] = consoleJSON(response) }()
	}
	var output []json.RawMessage
	if raw := target["output"]; !emptyConsoleJSON(raw) {
		if err := json.Unmarshal(raw, &output); err != nil {
			return nil, fmt.Errorf("解析 Console Responses 完成事件 output: %w", err)
		}
	}
	output = append(output, consoleJSON(consoleImageMessageItem(terminal)))
	target["output"] = consoleJSON(output)
	return &terminal, nil
}

func consoleMessageOutputIndex(payload map[string]json.RawMessage, messageID string) int {
	target := payload
	if raw := payload["response"]; !emptyConsoleJSON(raw) {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil {
			target = response
		}
	}
	var output []json.RawMessage
	_ = json.Unmarshal(target["output"], &output)
	for index, raw := range output {
		var item struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &item) == nil && item.ID == messageID {
			return index
		}
	}
	return len(output)
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
	localized := make([]consoleLocalizedImage, 0)
	for _, item := range output {
		if f.isInjectedImageCall(item) {
			image, err := f.localizeImageCall(ctx, item)
			if err != nil {
				return err
			}
			localized = append(localized, image)
			continue
		}
		if f.isInternalCall(item) {
			continue
		}
		filtered = append(filtered, item)
	}
	filtered = f.mergeLocalizedMediaIntoOutput(filtered, localized)
	envelope["output"] = consoleJSON(filtered)
	return nil
}

func (f *consoleHostedSearchFilter) attachPendingMediaToTextDone(eventType string, payload map[string]json.RawMessage) map[string]any {
	if eventType != "response.output_text.done" || len(f.pendingMedia) == 0 {
		return nil
	}
	var itemID string
	_ = json.Unmarshal(payload["item_id"], &itemID)
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil
	}
	media := make([]string, 0, len(f.pendingMedia))
	for _, localized := range f.pendingMedia {
		media = append(media, localized.text)
		f.mediaTargets[localized.messageID] = itemID
		f.messageMedia[itemID] = append(f.messageMedia[itemID], localized.text)
	}
	f.pendingMedia = nil
	mediaText := strings.Join(media, "\n\n")

	var text string
	_ = json.Unmarshal(payload["text"], &text)
	delta := mediaText
	if strings.TrimSpace(text) != "" {
		delta = "\n\n" + mediaText
	}
	payload["text"] = consoleJSON(appendConsoleMediaText(text, mediaText))

	outputIndex, _ := consoleJSONInt(payload["output_index"])
	contentIndex, _ := consoleJSONInt(payload["content_index"])
	return map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       itemID,
		"output_index":  outputIndex,
		"content_index": contentIndex,
		"delta":         delta,
		"logprobs":      []any{},
	}
}

func (f *consoleHostedSearchFilter) appendMediaToTerminalPayload(eventType string, payload map[string]json.RawMessage) {
	switch eventType {
	case "response.content_part.done":
		var itemID string
		_ = json.Unmarshal(payload["item_id"], &itemID)
		mediaText := strings.Join(f.messageMedia[strings.TrimSpace(itemID)], "\n\n")
		if mediaText == "" {
			return
		}
		var part map[string]any
		if json.Unmarshal(payload["part"], &part) != nil {
			return
		}
		text, _ := part["text"].(string)
		part["text"] = appendConsoleMediaText(text, mediaText)
		payload["part"] = consoleJSON(part)
	case "response.output_item.done":
		var item map[string]any
		if json.Unmarshal(payload["item"], &item) != nil {
			return
		}
		itemID, _ := item["id"].(string)
		mediaText := strings.Join(f.messageMedia[strings.TrimSpace(itemID)], "\n\n")
		if mediaText == "" || !appendConsoleMediaToMessage(item, mediaText) {
			return
		}
		payload["item"] = consoleJSON(item)
	}
}

func (f *consoleHostedSearchFilter) mergeLocalizedMediaIntoOutput(output []json.RawMessage, localized []consoleLocalizedImage) []json.RawMessage {
	if len(localized) == 0 {
		return output
	}
	byTarget := make(map[string][]string)
	unassigned := make([]string, 0)
	for _, image := range localized {
		if target := strings.TrimSpace(f.mediaTargets[image.messageID]); target != "" {
			byTarget[target] = append(byTarget[target], image.text)
		} else {
			unassigned = append(unassigned, image.text)
		}
	}

	lastAssistant := ""
	for _, raw := range output {
		var item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Type == "message" && item.Role == "assistant" {
			lastAssistant = strings.TrimSpace(item.ID)
		}
	}
	if len(unassigned) > 0 && lastAssistant != "" {
		byTarget[lastAssistant] = append(byTarget[lastAssistant], unassigned...)
		unassigned = nil
	}

	merged := make([]json.RawMessage, 0, len(output)+1)
	for _, raw := range output {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil {
			merged = append(merged, raw)
			continue
		}
		itemID, _ := item["id"].(string)
		mediaText := strings.Join(byTarget[strings.TrimSpace(itemID)], "\n\n")
		if mediaText != "" {
			appendConsoleMediaToMessage(item, mediaText)
			raw = consoleJSON(item)
		}
		merged = append(merged, raw)
	}
	if len(unassigned) > 0 {
		merged = append(merged, consoleJSON(consoleImageMessageItem(consoleLocalizedImage{
			messageID: "msg_grok2api_media_final",
			text:      strings.Join(unassigned, "\n\n"),
		})))
	}
	return merged
}

func appendConsoleMediaToMessage(item map[string]any, mediaText string) bool {
	if item["type"] != "message" || item["role"] != "assistant" {
		return false
	}
	content, ok := item["content"].([]any)
	if !ok {
		return false
	}
	for index := len(content) - 1; index >= 0; index-- {
		part, ok := content[index].(map[string]any)
		if !ok || part["type"] != "output_text" {
			continue
		}
		text, _ := part["text"].(string)
		part["text"] = appendConsoleMediaText(text, mediaText)
		content[index] = part
		item["content"] = content
		return true
	}
	item["content"] = append(content, map[string]any{
		"type": "output_text", "text": mediaText, "annotations": []any{}, "logprobs": []any{},
	})
	return true
}

func appendConsoleMediaText(text, mediaText string) string {
	mediaText = strings.TrimSpace(mediaText)
	if mediaText == "" || strings.Contains(text, mediaText) {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return mediaText
	}
	return text + "\n\n" + mediaText
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
