package acp

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	acpschema "github.com/gluonfield/acp-transport/acp"
	"github.com/wins/jaz/backend/internal/sessionevents"
)

const (
	maxToolContentBlocks  = 64
	maxToolContentText    = 8000
	maxToolRawInputBytes  = 2048
	maxToolRawOutputBytes = 8192
)

var (
	oauthJSONFieldPattern = regexp.MustCompile(`(?i)("(?:(?:access|refresh|id)_token|client_secret)"\s*:\s*")[^"]+(")`)
	bearerTokenPattern    = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/\-]+=*`)
	googleAccessPattern   = regexp.MustCompile(`\bya29\.[A-Za-z0-9._\-]+`)
	googleRefreshPattern  = regexp.MustCompile(`\b1//[A-Za-z0-9._\-]+`)
	// A token split across appended output chunks continues after its redacted start.
	redactedTailPattern = regexp.MustCompile(`\[REDACTED\][A-Za-z0-9._~+/\-]+=*`)
)

// mergeToolCall overlays the populated fields of src onto dst. ACP updates (and
// native local-agent updates) are sparse — only changed fields are present — so
// an empty field never clears what an earlier update already established. This is
// the single merge rule shared by the external-ACP and native paths.
func mergeToolCall(dst *sessionevents.ACPToolCall, src sessionevents.ACPToolCall) {
	if src.ID != "" {
		dst.ID = src.ID
	}
	if src.Title != "" {
		dst.Title = src.Title
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.Kind != "" {
		dst.Kind = src.Kind
	}
	if src.ToolName != "" {
		dst.ToolName = src.ToolName
	}
	dst.Cancellable = dst.Cancellable || src.Cancellable
	if len(src.Content) > 0 {
		dst.Content = src.Content
	}
	if len(src.Locations) > 0 {
		dst.Locations = src.Locations
	}
	if len(src.RawInput) > 0 {
		dst.RawInput = src.RawInput
	}
	if len(src.RawOutput) > 0 {
		dst.RawOutput = src.RawOutput
	}
	mergeACPToolRuntime(&dst.Runtime, src.Runtime)
	if !src.StartedAt.IsZero() && dst.StartedAt.IsZero() {
		dst.StartedAt = src.StartedAt
	}
	if !src.UpdatedAt.IsZero() {
		dst.UpdatedAt = src.UpdatedAt
	}
	normalizeToolCall(dst)
}

// toolUpdateSnapshot decodes one ACP tool-call update into a partial snapshot.
// A ToolCall converts to a ToolCallUpdate, so every path shares one merge.
func toolUpdateSnapshot(update acpschema.ToolCallUpdate) sessionevents.ACPToolCall {
	src := sessionevents.ACPToolCall{
		ID:        string(update.ToolCallID),
		Title:     update.Title,
		Kind:      derefString(update.Kind),
		ToolName:  toolUpdateName(update),
		Content:   normalizeToolContent(update.Content),
		Locations: normalizeToolLocations(update.Locations),
		RawInput:  boundedRawInput(update.RawInput),
		RawOutput: boundedRawOutput(update.RawOutput),
		Runtime:   acpToolRuntime(update.Meta),
	}
	if update.Status != nil {
		src.Status = string(*update.Status)
	}
	normalizeToolCall(&src)
	return src
}

func derefString[T ~string](value *T) string {
	if value == nil {
		return ""
	}
	return string(*value)
}

func boundedRawInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || len(raw) > maxToolRawInputBytes {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		return nil
	}
	return redactToolMap(out)
}

// boundedRawOutput keeps the tail of a text result, such as a command's whole
// output, and drops a structured result that is too large to show.
func boundedRawOutput(raw json.RawMessage) json.RawMessage {
	var text string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil
		}
		data, _ := json.Marshal(tailToolText(text))
		return data
	}
	if len(raw) == 0 || len(raw) > maxToolRawOutputBytes {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	data, err := json.Marshal(redactToolValue(value))
	if err != nil || len(data) == 0 || len(data) > maxToolRawOutputBytes {
		return nil
	}
	return json.RawMessage(data)
}

func normalizeToolLocations(locations []acpschema.ToolCallLocation) []sessionevents.ACPToolLocation {
	out := make([]sessionevents.ACPToolLocation, 0, len(locations))
	for _, location := range locations {
		if location.Path == "" {
			continue
		}
		out = append(out, sessionevents.ACPToolLocation{
			Path: redactToolText(location.Path),
			Line: location.Line,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toolUpdateName(update acpschema.ToolCallUpdate) string {
	if cc, ok := update.Meta["claudeCode"].(map[string]any); ok {
		if name, ok := cc["toolName"].(string); ok && name != "" {
			return name
		}
	}
	codex, ok := update.Meta[codexMetaKey].(map[string]any)
	if !ok {
		return ""
	}
	if _, ok := codex[providerSubagentsMetaKey]; !ok {
		return ""
	}
	var input struct {
		ActivityKind string `json:"activityKind"`
	}
	action := update.Title
	if json.Unmarshal(update.RawInput, &input) == nil && input.ActivityKind != "" {
		action = input.ActivityKind
	}
	if action != "" {
		return "codex." + action
	}
	return ""
}

func normalizeToolCall(call *sessionevents.ACPToolCall) {
	call.ToolName = normalizedToolName(call.ToolName, call.Kind, call.RawInput)
	webOutput := decodeWebToolOutput(call.ToolName, call.RawOutput)
	call.Title = webOutput.title(call.Title)
	if len(call.Content) == 0 {
		call.Content = webOutput.Results
	}
}

func normalizedToolName(name, kind string, input map[string]any) string {
	if name != "" {
		return name
	}
	variant, _ := input["variant"].(string)
	switch toolNameKey(variant) {
	case "bash":
		return "Bash"
	case "listdir":
		return "LS"
	case "readfile":
		return "Read"
	case "webfetch":
		return "WebFetch"
	case "websearch", "xsearch":
		return "WebSearch"
	}
	if kind != string(acpschema.ToolKindFetch) {
		return ""
	}
	action, _ := input["action"].(map[string]any)
	if action["type"] == "search" {
		return "WebSearch"
	}
	return "WebFetch"
}

func toolNameKey(name string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(name))
}

type webToolOutput struct {
	Query   string
	Results []sessionevents.ACPToolContent
}

func decodeWebToolOutput(toolName string, raw json.RawMessage) webToolOutput {
	if toolName != "WebSearch" && toolName != "WebFetch" || len(raw) == 0 {
		return webToolOutput{}
	}
	if raw[0] == '"' {
		var nested string
		if json.Unmarshal(raw, &nested) != nil {
			return webToolOutput{}
		}
		raw = json.RawMessage(nested)
	}
	var value struct {
		Action struct {
			Query   string `json:"query"`
			URL     string `json:"url"`
			Sources []struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"sources"`
		} `json:"action"`
		Input struct {
			Query string `json:"query"`
		} `json:"input"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return webToolOutput{}
	}
	out := webToolOutput{Query: firstNonEmpty(value.Action.Query, value.Input.Query)}
	seen := make(map[string]struct{})
	appendResult := func(uri, title string) {
		uri = redactToolText(uri)
		if uri == "" {
			return
		}
		if _, ok := seen[uri]; ok {
			return
		}
		seen[uri] = struct{}{}
		if len(out.Results) < maxToolContentBlocks {
			out.Results = append(out.Results, sessionevents.ACPToolContent{
				Type:  "link",
				URI:   uri,
				Title: redactToolText(title),
			})
		}
	}
	appendResult(value.Action.URL, "")
	for _, source := range value.Action.Sources {
		appendResult(source.URL, source.Title)
	}
	return out
}

func (o webToolOutput) title(title string) string {
	clean := strings.Trim(strings.TrimSpace(title), `"`)
	if o.Query != "" && (clean == "" || strings.EqualFold(clean, "web search") || strings.EqualFold(clean, "x search")) {
		return redactToolText(o.Query)
	}
	return title
}

func normalizeToolContent(items []acpschema.ToolCallContent) []sessionevents.ACPToolContent {
	out := make([]sessionevents.ACPToolContent, 0, len(items))
	for _, raw := range items {
		if len(out) >= maxToolContentBlocks {
			break
		}
		if block := decodeToolContent(json.RawMessage(raw)); block != nil {
			out = append(out, *block)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeToolContent flattens one ACP ToolCallContent (a "content" block wrapping
// a ContentBlock, or a "diff") into the normalized display shape. Decoding stays
// field-driven rather than depending on every acp-transport sub-struct so any
// agent's blocks survive.
func decodeToolContent(raw json.RawMessage) *sessionevents.ACPToolContent {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	switch env.Type {
	case "content":
		var wrap struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &wrap); err != nil || len(wrap.Content) == 0 {
			return nil
		}
		return decodeContentBlock(wrap.Content)
	case "diff":
		var d struct {
			Path    string `json:"path"`
			OldText string `json:"oldText"`
			NewText string `json:"newText"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil
		}
		return &sessionevents.ACPToolContent{
			Type:    "diff",
			Path:    redactToolText(d.Path),
			OldText: clampToolText(d.OldText),
			NewText: clampToolText(d.NewText),
		}
	default:
		return nil
	}
}

func decodeContentBlock(raw json.RawMessage) *sessionevents.ACPToolContent {
	var block struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		URI      string          `json:"uri"`
		Name     string          `json:"name"`
		Title    string          `json:"title"`
		Resource json.RawMessage `json:"resource"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil
	}
	switch block.Type {
	case "text":
		text := clampToolText(block.Text)
		if text == "" {
			return nil
		}
		return &sessionevents.ACPToolContent{Type: "text", Text: text}
	case "resource_link":
		if block.URI == "" {
			return nil
		}
		return &sessionevents.ACPToolContent{
			Type:  "link",
			URI:   redactToolText(block.URI),
			Title: redactToolText(firstNonEmpty(block.Title, block.Name)),
		}
	case "resource":
		var res struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		}
		if len(block.Resource) > 0 {
			_ = json.Unmarshal(block.Resource, &res)
		}
		uri := redactToolText(res.URI)
		if text := clampToolText(res.Text); text != "" {
			return &sessionevents.ACPToolContent{Type: "text", Text: text, URI: uri}
		}
		if uri != "" {
			return &sessionevents.ACPToolContent{Type: "link", URI: uri}
		}
		return nil
	default:
		// image / audio / unknown blocks carry no inline text worth showing.
		return nil
	}
}

func clampToolText(s string) string {
	s = redactToolText(s)
	if utf8.RuneCountInString(s) <= maxToolContentText {
		return s
	}
	return string([]rune(s)[:maxToolContentText]) + "…"
}

// tailToolText keeps the end of a command's output, where it reports its result.
func tailToolText(s string) string {
	s = redactToolText(s)
	runes := []rune(s)
	if len(runes) <= maxToolContentText {
		return s
	}
	return "…" + string(runes[len(runes)-maxToolContentText:])
}

func redactToolText(s string) string {
	s = oauthJSONFieldPattern.ReplaceAllString(s, `${1}[REDACTED]${2}`)
	s = bearerTokenPattern.ReplaceAllString(s, `${1}[REDACTED]`)
	s = googleAccessPattern.ReplaceAllString(s, `[REDACTED]`)
	s = googleRefreshPattern.ReplaceAllString(s, `[REDACTED]`)
	s = redactedTailPattern.ReplaceAllString(s, `[REDACTED]`)
	return s
}

func redactToolMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[redactToolText(key)] = redactToolValue(value)
	}
	return out
}

func redactToolValue(value any) any {
	switch v := value.(type) {
	case string:
		return redactToolText(v)
	case map[string]any:
		return redactToolMap(v)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, redactToolValue(item))
		}
		return out
	default:
		return value
	}
}
