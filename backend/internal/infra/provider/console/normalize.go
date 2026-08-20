package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

var (
	resetDurationPattern = regexp.MustCompile(`(?i)(\d+)\s*([dhms])`)
)

const (
	maxConsoleWebSearchDomains = 5
	maxConsoleXSearchHandles   = 20
	consoleViewImageToolName   = "view_image"
	consoleViewImageToolAlias  = "view_image_local_grok2api"
)

func normalizeRequest(body []byte, spec ModelSpec) ([]byte, error) {
	return normalizeRequestWithMetadata(body, spec, nil)
}

func normalizeRequestWithMetadata(body []byte, spec ModelSpec, metadata *provider.NormalizedRequestMetadata) ([]byte, error) {
	normalized, _, err := normalizeRequestWithRouteAndMetadata(body, spec, metadata)
	return normalized, err
}

type consoleHostedToolRoute struct {
	hasXSearch          bool
	aliasedViewImage    bool
	injectedToolTypes   map[string]struct{}
	clientDeclaredTools map[string]struct{}
}

func normalizeRequestWithRoute(body []byte, spec ModelSpec) ([]byte, consoleHostedToolRoute, error) {
	return normalizeRequestWithRouteAndMetadata(body, spec, nil)
}

func normalizeRequestWithRouteAndMetadata(body []byte, spec ModelSpec, metadata *provider.NormalizedRequestMetadata) ([]byte, consoleHostedToolRoute, error) {
	route := consoleHostedToolRoute{
		injectedToolTypes:   make(map[string]struct{}),
		clientDeclaredTools: make(map[string]struct{}),
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, route, fmt.Errorf("解析 Console Responses 请求: %w", err)
	}
	payload["model"] = spec.UpstreamModel
	// Console is stateless. Replay the supplied input and silently discard
	// stateful client hints instead of rejecting an otherwise valid request.
	payload["store"] = false
	for _, field := range []string{
		"metadata", "previous_response_id", "service_tier", "prompt_cache_key",
		"background", "conversation",
	} {
		delete(payload, field)
	}
	normalizeConsoleResponseFormat(payload)
	patchConsoleInput(payload)
	if _, exists := payload["max_output_tokens"]; !exists && spec.MaxOutputTokens > 0 {
		payload["max_output_tokens"] = spec.MaxOutputTokens
	}
	requestedEffort := metadata != nil && auditdomain.NormalizeReasoningEffort(metadata.ReasoningEffort) != ""
	requestedEffort = requestedEffort || hasRecognizedConsoleReasoningEffort(payload)
	normalizeReasoning(payload, spec)
	updateConsoleReasoningMetadata(payload, spec, requestedEffort, metadata)
	ensureReasoningInclude(payload)
	retainedClientTools, err := normalizeConsoleTools(payload)
	if err != nil {
		return nil, route, err
	}
	route = injectConsoleHostedTools(payload)
	normalizeConsoleToolChoice(payload, retainedClientTools)
	aliasConsoleReservedFunctionTools(payload, &route)
	normalized, err := json.Marshal(payload)
	return normalized, route, err
}

func hasRecognizedConsoleReasoningEffort(payload map[string]any) bool {
	reasoning, _ := payload["reasoning"].(map[string]any)
	effort, _ := reasoning["effort"].(string)
	if strings.EqualFold(strings.TrimSpace(effort), "auto") {
		return true
	}
	return normalizeEffort(effort) != ""
}

func updateConsoleReasoningMetadata(payload map[string]any, spec ModelSpec, requested bool, metadata *provider.NormalizedRequestMetadata) {
	if metadata == nil {
		return
	}
	previous := auditdomain.NormalizeReasoningEffort(metadata.ReasoningEffort)
	metadata.ReasoningEffort = ""
	if !requested || !spec.SupportsReasoning {
		return
	}
	if !spec.SupportsReasoningEffort {
		metadata.ReasoningEffort = "fixed"
		return
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	effort, _ := reasoning["effort"].(string)
	if normalized := auditdomain.NormalizeReasoningEffort(effort); normalized != "" {
		metadata.ReasoningEffort = normalized
		return
	}
	metadata.ReasoningEffort = previous
}

func normalizeConsoleResponseFormat(payload map[string]any) {
	raw, exists := payload["response_format"]
	if !exists {
		return
	}
	delete(payload, "response_format")
	format, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if typeName, _ := format["type"].(string); typeName == "json_schema" {
		if nested, ok := format["json_schema"].(map[string]any); ok {
			flattened := map[string]any{"type": "json_schema"}
			for key, value := range nested {
				if key != "type" {
					flattened[key] = value
				}
			}
			format = flattened
		}
	}
	text, _ := payload["text"].(map[string]any)
	if text == nil {
		text = make(map[string]any)
	}
	if _, exists := text["format"]; !exists {
		text["format"] = format
	}
	payload["text"] = text
}

func patchConsoleInput(payload map[string]any) {
	items, ok := payload["input"].([]any)
	if !ok {
		return
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "reasoning" {
			patchConsoleReasoningContent(item)
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := part["type"].(string)
			switch typeName {
			case "text", "output_text":
				part["type"] = "input_text"
			case "image_url":
				if image, ok := part["image_url"].(map[string]any); ok {
					if url, _ := image["url"].(string); strings.TrimSpace(url) != "" {
						part["type"] = "input_image"
						part["image_url"] = url
					}
				}
			}
		}
	}
}

func patchConsoleReasoningContent(item map[string]any) {
	content, ok := item["content"].([]any)
	if !ok {
		return
	}
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := part["type"]; !exists {
			if _, hasText := part["text"]; hasText {
				part["type"] = "reasoning_text"
			}
		}
	}
}

func normalizeReasoning(payload map[string]any, spec ModelSpec) {
	if !spec.SupportsReasoning {
		delete(payload, "reasoning")
		return
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = make(map[string]any)
	}
	if summary, _ := reasoning["summary"].(string); strings.TrimSpace(summary) == "" {
		reasoning["summary"] = "detailed"
	}
	if !spec.SupportsReasoningEffort {
		delete(reasoning, "effort")
		payload["reasoning"] = reasoning
		return
	}
	effort, _ := reasoning["effort"].(string)
	effort = normalizeEffort(effort)
	if effort == "" {
		effort = spec.DefaultReasoningEffort
	}
	if effort != "" {
		reasoning["effort"] = effort
	}
	payload["reasoning"] = reasoning
}

func normalizeEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max":
		return "xhigh"
	default:
		return ""
	}
}

func ensureReasoningInclude(payload map[string]any) {
	value, _ := payload["include"].([]any)
	seen := make(map[string]struct{})
	result := make([]any, 0)
	for _, item := range value {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	if _, exists := seen["reasoning.encrypted_content"]; !exists {
		result = append(result, "reasoning.encrypted_content")
	}
	payload["include"] = result
}

func normalizeConsoleTools(payload map[string]any) (bool, error) {
	value, exists := payload["tools"]
	if !exists || value == nil {
		delete(payload, "tools")
		return false, nil
	}
	tools, ok := value.([]any)
	if !ok {
		delete(payload, "tools")
		return false, nil
	}
	result := make([]any, 0, len(tools))
	retainedClientTools := false
	seenCodeExecution := false
	for index, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := tool["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typeName)) {
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
			clean := map[string]any{"type": "web_search", "enable_image_understanding": true}
			if enabled, ok := tool["enable_image_understanding"].(bool); ok {
				clean["enable_image_understanding"] = enabled
			}
			if enabled, ok := tool["enable_image_search"].(bool); ok {
				clean["enable_image_search"] = enabled
			}
			allowed, excluded, err := normalizeConsoleWebSearchDomains(tool, index)
			if err != nil {
				return false, err
			}
			if len(allowed) > 0 {
				clean["allowed_domains"] = allowed
			}
			if len(excluded) > 0 {
				clean["excluded_domains"] = excluded
			}
			result = append(result, clean)
		case "x_search":
			clean := map[string]any{"type": "x_search", "enable_video_understanding": true}
			allowed, err := normalizeConsoleStringList(tool["allowed_x_handles"], maxConsoleXSearchHandles, fmt.Sprintf("tools[%d].allowed_x_handles", index))
			if err != nil {
				return false, err
			}
			excluded, err := normalizeConsoleStringList(tool["excluded_x_handles"], maxConsoleXSearchHandles, fmt.Sprintf("tools[%d].excluded_x_handles", index))
			if err != nil {
				return false, err
			}
			if len(allowed) > 0 && len(excluded) > 0 {
				return false, fmt.Errorf("tools[%d] 不能同时设置 allowed_x_handles 和 excluded_x_handles", index)
			}
			if len(allowed) > 0 {
				clean["allowed_x_handles"] = allowed
			}
			if len(excluded) > 0 {
				clean["excluded_x_handles"] = excluded
			}
			if enabled, ok := tool["enable_image_understanding"].(bool); ok {
				clean["enable_image_understanding"] = enabled
			}
			if enabled, ok := tool["enable_video_understanding"].(bool); ok {
				clean["enable_video_understanding"] = enabled
			}
			parsedBounds := make(map[string]time.Time, 2)
			for _, field := range []string{"from_date", "to_date"} {
				text, exists := tool[field]
				if !exists || text == nil {
					continue
				}
				value, ok := text.(string)
				if !ok || strings.TrimSpace(value) == "" {
					return false, fmt.Errorf("tools[%d].%s 必须是 ISO 8601 时间字符串", index, field)
				}
				value = strings.TrimSpace(value)
				parsed, err := parseConsoleSearchTime(value)
				if err != nil {
					return false, fmt.Errorf("tools[%d].%s 必须是 ISO 8601 时间字符串", index, field)
				}
				clean[field] = value
				parsedBounds[field] = parsed
			}
			from, hasFrom := parsedBounds["from_date"]
			to, hasTo := parsedBounds["to_date"]
			if hasFrom && hasTo {
				if from.After(to) {
					return false, fmt.Errorf("tools[%d].from_date 不能晚于 to_date", index)
				}
			}
			result = append(result, clean)
		case "function":
			name, _ := tool["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			clean := map[string]any{"type": "function", "name": strings.TrimSpace(name)}
			for _, field := range []string{"description", "parameters", "strict"} {
				if fieldValue, exists := tool[field]; exists {
					clean[field] = fieldValue
				}
			}
			result = append(result, clean)
			retainedClientTools = true
		case "image_generation":
			// Match the Console Responses contract: image generation accepts only
			// the optional auto/generate/edit action. Unknown fields are dropped so
			// clients cannot accidentally forward an incompatible Images API schema.
			clean := map[string]any{"type": "image_generation"}
			if action, _ := tool["action"].(string); action != "" {
				switch action = strings.TrimSpace(action); action {
				case "auto", "generate", "edit":
					clean["action"] = action
				}
			}
			result = append(result, clean)
			retainedClientTools = true
		case "code_execution", "code_interpreter":
			// xAI accepts code_execution as the request tool type and emits
			// code_interpreter_call output items while the hosted tool runs.
			// Normalize the common alias and collapse duplicates before the
			// Console defaults are mounted.
			if !seenCodeExecution {
				result = append(result, map[string]any{"type": "code_execution"})
				seenCodeExecution = true
			}
			retainedClientTools = true
		case "mcp", "shell", "collections_search", "file_search":
			// These are native xAI Responses tool variants. Keep their payloads,
			// while namespace/tool_search remain client-side abstractions and are
			// intentionally omitted instead of causing an upstream 400.
			result = append(result, tool)
			retainedClientTools = true
		}
	}
	if len(result) == 0 {
		delete(payload, "tools")
		return false, nil
	}
	payload["tools"] = result
	return retainedClientTools, nil
}

func normalizeConsoleWebSearchDomains(tool map[string]any, index int) ([]string, []string, error) {
	filters, _ := tool["filters"].(map[string]any)
	allowed, err := normalizeConsoleMergedStringList(tool["allowed_domains"], filters["allowed_domains"], maxConsoleWebSearchDomains, fmt.Sprintf("tools[%d].allowed_domains", index))
	if err != nil {
		return nil, nil, err
	}
	excluded, err := normalizeConsoleMergedStringList(tool["excluded_domains"], filters["excluded_domains"], maxConsoleWebSearchDomains, fmt.Sprintf("tools[%d].excluded_domains", index))
	if err != nil {
		return nil, nil, err
	}
	if len(allowed) > 0 && len(excluded) > 0 {
		return nil, nil, fmt.Errorf("tools[%d] 不能同时设置 allowed_domains 和 excluded_domains", index)
	}
	return allowed, excluded, nil
}

func normalizeConsoleMergedStringList(topLevel, nested any, limit int, field string) ([]string, error) {
	top, err := normalizeConsoleStringList(topLevel, limit, field)
	if err != nil {
		return nil, err
	}
	inner, err := normalizeConsoleStringList(nested, limit, field)
	if err != nil {
		return nil, err
	}
	if len(top) > 0 && len(inner) > 0 && !equalConsoleStringLists(top, inner) {
		return nil, fmt.Errorf("%s 的顶层声明与 filters 声明冲突", field)
	}
	if len(top) > 0 {
		return top, nil
	}
	return inner, nil
}

func normalizeConsoleStringList(value any, limit int, field string) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	var raw []any
	switch typed := value.(type) {
	case []any:
		raw = typed
	case []string:
		raw = make([]any, len(typed))
		for index := range typed {
			raw[index] = typed[index]
		}
	default:
		return nil, fmt.Errorf("%s 必须是字符串数组", field)
	}
	if limit > 0 && len(raw) > limit {
		return nil, fmt.Errorf("%s 不能超过 %d 项", field, limit)
	}
	result := make([]string, 0, len(raw))
	for index, item := range raw {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%s[%d] 必须是非空字符串", field, index)
		}
		result = append(result, strings.TrimSpace(text))
	}
	return result, nil
}

func equalConsoleStringLists(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func parseConsoleSearchTime(value string) (time.Time, error) {
	if parsed, err := time.Parse("2006-01-02", value); err == nil && parsed.Format("2006-01-02") == value {
		return parsed, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

// injectConsoleHostedTools mounts xAI's provider-hosted tools on every
// conversation request that has already been routed to Console. This runs in
// the Console adapter rather than model-name parsing so both explicit
// Console/grok-* requests and unprefixed models selected for Console behave the
// same way, including requests converted from Chat Completions or Messages.
func injectConsoleHostedTools(payload map[string]any) consoleHostedToolRoute {
	route := consoleHostedToolRoute{
		injectedToolTypes:   make(map[string]struct{}),
		clientDeclaredTools: make(map[string]struct{}),
	}
	tools, _ := payload["tools"].([]any)
	seenWebSearch := false
	seenXSearch := false
	seenImageGeneration := false
	seenCodeExecution := false
	for _, rawTool := range tools {
		identity := strings.ToLower(strings.TrimSpace(toolIdentity(rawTool)))
		switch identity {
		case "web_search":
			seenWebSearch = true
		case "x_search":
			seenXSearch = true
		case "image_generation":
			seenImageGeneration = true
		case "code_execution":
			seenCodeExecution = true
		default:
			if name, ok := strings.CutPrefix(identity, "function:"); ok && name != "" {
				route.clientDeclaredTools[name] = struct{}{}
			}
		}
	}
	if !seenWebSearch {
		tools = append(tools, map[string]any{
			"type":                       "web_search",
			"enable_image_understanding": true,
		})
		route.injectedToolTypes["web_search"] = struct{}{}
	}
	if !seenXSearch {
		tools = append(tools, map[string]any{
			"type":                       "x_search",
			"enable_video_understanding": true,
		})
		route.injectedToolTypes["x_search"] = struct{}{}
	}
	if !seenImageGeneration {
		tools = append(tools, map[string]any{
			"type":   "image_generation",
			"action": "auto",
		})
		route.injectedToolTypes["image_generation"] = struct{}{}
	}
	if !seenCodeExecution {
		tools = append(tools, map[string]any{"type": "code_execution"})
		route.injectedToolTypes["code_execution"] = struct{}{}
	}
	route.hasXSearch = true
	payload["tools"] = tools
	return route
}

func aliasConsoleReservedFunctionTools(payload map[string]any, route *consoleHostedToolRoute) {
	if route == nil {
		return
	}
	tools, _ := payload["tools"].([]any)
	if !hasConsoleToolType(tools, "web_search") || !hasConsoleFunctionTool(tools, consoleViewImageToolName) {
		return
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		if strings.EqualFold(strings.TrimSpace(typeName), "function") && strings.EqualFold(strings.TrimSpace(name), consoleViewImageToolName) {
			tool["name"] = consoleViewImageToolAlias
			route.aliasedViewImage = true
		}
	}
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		typeName, _ := choice["type"].(string)
		name, _ := choice["name"].(string)
		if strings.EqualFold(strings.TrimSpace(typeName), "function") && strings.EqualFold(strings.TrimSpace(name), consoleViewImageToolName) {
			choice["name"] = consoleViewImageToolAlias
		}
	}
}

func hasConsoleToolType(tools []any, target string) bool {
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := tool["type"].(string)
		if strings.EqualFold(strings.TrimSpace(typeName), target) {
			return true
		}
	}
	return false
}

func hasConsoleFunctionTool(tools []any, target string) bool {
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		if strings.EqualFold(strings.TrimSpace(typeName), "function") && strings.EqualFold(strings.TrimSpace(name), target) {
			return true
		}
	}
	return false
}

func normalizeConsoleToolChoice(payload map[string]any, retainedClientTools bool) {
	if _, exists := payload["tools"]; !exists {
		delete(payload, "tool_choice")
		return
	}
	choice, exists := payload["tool_choice"]
	if !exists {
		payload["tool_choice"] = "auto"
		return
	}
	if value, ok := choice.(string); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "none", "auto":
			payload["tool_choice"] = strings.ToLower(strings.TrimSpace(value))
		case "required":
			if retainedClientTools {
				payload["tool_choice"] = "required"
			} else {
				payload["tool_choice"] = "auto"
			}
		default:
			payload["tool_choice"] = "auto"
		}
		return
	}
	object, ok := choice.(map[string]any)
	if !ok {
		payload["tool_choice"] = "auto"
		return
	}
	typeName, _ := object["type"].(string)
	typeName = strings.ToLower(strings.TrimSpace(typeName))
	if typeName != "function" || !retainedClientTools {
		payload["tool_choice"] = "auto"
		return
	}
	name, _ := object["name"].(string)
	if strings.TrimSpace(name) == "" {
		if function, ok := object["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
		}
	}
	if strings.TrimSpace(name) == "" {
		payload["tool_choice"] = "auto"
		return
	}
	payload["tool_choice"] = map[string]any{"type": "function", "name": strings.TrimSpace(name)}
}

func toolIdentity(value any) string {
	tool, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	typeName, _ := tool["type"].(string)
	if typeName != "function" {
		return typeName
	}
	name, _ := tool["name"].(string)
	return typeName + ":" + name
}

func consoleRetryAfter(body []byte) time.Duration {
	text := string(body)
	index := strings.Index(strings.ToLower(text), "resets in:")
	if index < 0 {
		return 0
	}
	text = text[index+len("resets in:"):]
	var total time.Duration
	for _, match := range resetDurationPattern.FindAllStringSubmatch(text, -1) {
		value, _ := strconv.Atoi(match[1])
		switch strings.ToLower(match[2]) {
		case "d":
			total += time.Duration(value) * 24 * time.Hour
		case "h":
			total += time.Duration(value) * time.Hour
		case "m":
			total += time.Duration(value) * time.Minute
		case "s":
			total += time.Duration(value) * time.Second
		}
	}
	return total
}

func parseConsoleRetryAfterHeader(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func parseConsoleRateLimitMetadata(body []byte) *provider.RateLimitMetadata {
	return provider.ParseRateLimitMetadata(body)
}
