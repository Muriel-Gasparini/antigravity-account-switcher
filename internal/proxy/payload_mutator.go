package proxy

import "encoding/json"

// PartVisitor is a function invoked for each part in a conversation message turn.
// It returns keep (whether to keep this part in the parts slice) and modified (whether the part was modified).
type PartVisitor func(role string, part map[string]any) (keep bool, modified bool)

// GetRequestMap safely retrieves the nested "request" object from a root-level JSON document.
func GetRequestMap(doc map[string]any) (map[string]any, bool) {
	if doc == nil {
		return nil, false
	}
	req, ok := doc["request"].(map[string]any)
	return req, ok
}

// ResolveRequestMap extracts the inner request map from doc.
//  1. If doc contains a nested "request" map, that map is returned.
//  2. If doc contains a "request" key that is NOT a map, returns nil, false.
//  3. If doc does NOT contain a "request" key, but contains request fields ("contents", "generationConfig", or "labels"),
//     doc itself is treated as the request map.
//  4. Otherwise, returns nil, false.
func ResolveRequestMap(doc map[string]any) (map[string]any, bool) {
	if doc == nil {
		return nil, false
	}
	if req, ok := doc["request"].(map[string]any); ok {
		return req, true
	}
	if _, hasReq := doc["request"]; hasReq {
		return nil, false
	}
	if _, hasContents := doc["contents"]; hasContents {
		return doc, true
	}
	if _, hasGenCfg := doc["generationConfig"]; hasGenCfg {
		return doc, true
	}
	if _, hasLabels := doc["labels"]; hasLabels {
		return doc, true
	}
	return nil, false
}

// GetGenerationConfig safely retrieves the "generationConfig" object from a request or document map.
func GetGenerationConfig(req map[string]any) (map[string]any, bool) {
	if req == nil {
		return nil, false
	}
	if cfg, ok := req["generationConfig"].(map[string]any); ok {
		return cfg, true
	}
	if innerReq, ok := req["request"].(map[string]any); ok {
		return GetGenerationConfig(innerReq)
	}
	return nil, false
}

// GetLabels safely retrieves the "labels" object from a request or document map.
func GetLabels(req map[string]any) (map[string]any, bool) {
	if req == nil {
		return nil, false
	}
	if labels, ok := req["labels"].(map[string]any); ok {
		return labels, true
	}
	if innerReq, ok := req["request"].(map[string]any); ok {
		return GetLabels(innerReq)
	}
	return nil, false
}

// toInt converts numeric values (int, int32, int64, float64, float32, json.Number) to int safely.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		if f, err := n.Float64(); err == nil {
			return int(f), true
		}
	}
	return 0, false
}

// toFloat64 converts numeric values (float64, float32, int, int32, int64, json.Number) to float64 safely.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

// ClampMaxOutputTokens clamps maxOutputTokens in generationConfig to maxOut if current value exceeds it.
// Supports float64, float32, int, int32, int64, and json.Number representations.
func ClampMaxOutputTokens(req map[string]any, maxOut int) bool {
	genCfg, ok := GetGenerationConfig(req)
	if !ok {
		return false
	}
	if currMax, ok := toInt(genCfg["maxOutputTokens"]); ok && currMax > maxOut {
		genCfg["maxOutputTokens"] = maxOut
		return true
	}
	return false
}

// UpdateModelEnumLabel updates labels["model_enum"] to match ModelPlaceholderMap for model if registered.
func UpdateModelEnumLabel(labels map[string]any, model string) bool {
	if placeholder, exists := ModelPlaceholderMap[model]; exists {
		if labels["model_enum"] != placeholder {
			labels["model_enum"] = placeholder
			return true
		}
	}
	return false
}

// MutateParts traverses contents[i].parts[j] within a request or document map, applying fn to each part.
// If all parts in a message turn are removed, a fallback {"text": ""} part is injected so that
// the message turn never has an empty parts array.
// Returns true if any part was modified, dropped, or if a fallback part was injected.
func MutateParts(doc map[string]any, fn PartVisitor) bool {
	if doc == nil || fn == nil {
		return false
	}

	target, ok := ResolveRequestMap(doc)
	if !ok {
		return false
	}

	rawContents, ok := target["contents"]
	if !ok || rawContents == nil {
		return false
	}

	contents, ok := rawContents.([]any)
	if !ok {
		return false
	}

	changed := false
	for _, c := range contents {
		cMap, ok := c.(map[string]any)
		if !ok {
			continue
		}

		rawParts, ok := cMap["parts"]
		if !ok || rawParts == nil {
			continue
		}

		parts, ok := rawParts.([]any)
		if !ok {
			continue
		}

		filteredParts := make([]any, 0, len(parts))
		turnModified := false

		role, _ := cMap["role"].(string)

		for _, p := range parts {
			pMap, ok := p.(map[string]any)
			if !ok {
				filteredParts = append(filteredParts, p)
				continue
			}

			keep, modified := fn(role, pMap)
			if modified {
				turnModified = true
			}
			if keep {
				filteredParts = append(filteredParts, pMap)
			} else {
				turnModified = true
			}
		}

		if len(filteredParts) == 0 && len(parts) > 0 {
			filteredParts = append(filteredParts, map[string]any{"text": ""})
			turnModified = true
		}

		if turnModified {
			cMap["parts"] = filteredParts
			changed = true
		}
	}

	return changed
}
