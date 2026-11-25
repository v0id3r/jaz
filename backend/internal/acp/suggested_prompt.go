package acp

import "strings"

// mapValue casts a decoded JSON value to the object form.
func mapValue(value any) (map[string]any, bool) {
	out, ok := value.(map[string]any)
	return out, ok
}

// suggestedPrompt reads the prompt an agent predicts the user will send next,
// which Claude's adapter publishes as session_info_update._meta.promptSuggestions.
// ok is false when the update says nothing about it; an empty list clears it.
func suggestedPrompt(meta map[string]any) (string, bool) {
	value, ok := mapValue(meta["promptSuggestions"])
	if !ok {
		return "", false
	}
	suggestions, _ := value["suggestions"].([]any)
	for _, item := range suggestions {
		suggestion, _ := mapValue(item)
		if text := strings.TrimSpace(stringValue(suggestion["text"])); text != "" {
			return text, true
		}
	}
	return "", true
}
