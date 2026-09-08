package proxy

// PayloadAdapter defines the strategy interface for adapting request payloads
// across different model families (e.g. Claude/GPT, Gemini).
type PayloadAdapter interface {
	Adapt(doc map[string]any, targetModel string) (changed bool, err error)
}

// ClaudePayloadAdapter adapts payloads destined for Anthropic Claude or OpenAI GPT models.
// - Clamps maxOutputTokens to model limits (<= 64k for Claude, <= 32k for GPT).
// - Ensures thinkingBudget is at least 1024 when thinkingConfig is enabled.
// - Strips unauthenticated thought blocks and thoughtSignature fields.
// - Sets used_claude telemetry labels and model_enum placeholder.
type ClaudePayloadAdapter struct{}

func (a *ClaudePayloadAdapter) Adapt(doc map[string]any, targetModel string) (bool, error) {
	req, ok := ResolveRequestMap(doc)
	if !ok {
		return false, nil
	}

	cleanTarget := NormalizeModelName(targetModel)
	if cleanTarget == "" {
		return false, nil
	}

	maxOut := MaxOutputTokensForModel(cleanTarget)
	changed := false

	if ClampMaxOutputTokens(req, maxOut) {
		changed = true
	}

	if genCfg, ok := GetGenerationConfig(req); ok {
		if thkCfg, ok := genCfg["thinkingConfig"].(map[string]any); ok {
			if budget, ok := toFloat64(thkCfg["thinkingBudget"]); ok && budget < 1024 {
				thkCfg["thinkingBudget"] = 1024
				changed = true
			}
		}
	}

	partsChanged := MutateParts(req, func(role string, part map[string]any) (keep bool, modified bool) {
		if isThought, _ := part["thought"].(bool); isThought {
			return false, true
		}
		if _, hasSig := part["thoughtSignature"]; hasSig {
			delete(part, "thoughtSignature")
			return true, true
		}
		return true, false
	})
	if partsChanged {
		changed = true
	}

	if labels, ok := GetLabels(req); ok {
		if labels["used_claude"] != "true" {
			labels["used_claude"] = "true"
			changed = true
		}
		if labels["used_claude_conservative"] != "true" {
			labels["used_claude_conservative"] = "true"
			changed = true
		}
		if labels["used_non_gemini_model"] != "true" {
			labels["used_non_gemini_model"] = "true"
			changed = true
		}
		if UpdateModelEnumLabel(labels, cleanTarget) {
			changed = true
		}
	}

	return changed, nil
}

// GeminiPayloadAdapter adapts payloads destined for Google Gemini models.
// - Clamps maxOutputTokens to model limits (<= 65536).
// - Strips invalid thought blocks from other vendors.
// - Ensures functionCall parts have skip_thought_signature_validator if signature is missing.
// - Sets used_claude telemetry labels to false and model_enum placeholder.
type GeminiPayloadAdapter struct{}

func (a *GeminiPayloadAdapter) Adapt(doc map[string]any, targetModel string) (bool, error) {
	req, ok := ResolveRequestMap(doc)
	if !ok {
		return false, nil
	}

	cleanTarget := NormalizeModelName(targetModel)
	if cleanTarget == "" {
		return false, nil
	}

	maxOut := MaxOutputTokensForModel(cleanTarget)
	changed := false

	if ClampMaxOutputTokens(req, maxOut) {
		changed = true
	}

	partsChanged := MutateParts(req, func(role string, part map[string]any) (keep bool, modified bool) {
		if isThought, _ := part["thought"].(bool); isThought {
			return false, true
		}
		if _, hasFunc := part["functionCall"]; hasFunc {
			sig, _ := part["thoughtSignature"].(string)
			if sig == "" {
				part["thoughtSignature"] = "skip_thought_signature_validator"
				return true, true
			}
		}
		return true, false
	})
	if partsChanged {
		changed = true
	}

	if labels, ok := GetLabels(req); ok {
		if labels["used_claude"] != "false" {
			labels["used_claude"] = "false"
			changed = true
		}
		if labels["used_claude_conservative"] != "false" {
			labels["used_claude_conservative"] = "false"
			changed = true
		}
		if labels["used_non_gemini_model"] != "false" {
			labels["used_non_gemini_model"] = "false"
			changed = true
		}
		if UpdateModelEnumLabel(labels, cleanTarget) {
			changed = true
		}
	}

	return changed, nil
}

// noopPayloadAdapter is a null-object implementation for unknown categories.
type noopPayloadAdapter struct{}

func (n *noopPayloadAdapter) Adapt(doc map[string]any, targetModel string) (bool, error) {
	return false, nil
}

var (
	defaultClaudeAdapter = &ClaudePayloadAdapter{}
	defaultGeminiAdapter = &GeminiPayloadAdapter{}
	defaultNoopAdapter   = &noopPayloadAdapter{}
)

// GetPayloadAdapter returns the appropriate PayloadAdapter for the given ModelCategory.
// Guaranteed to return a non-nil adapter.
func GetPayloadAdapter(category ModelCategory) PayloadAdapter {
	switch category {
	case CategoryClaudeGPT:
		return defaultClaudeAdapter
	case CategoryGemini:
		return defaultGeminiAdapter
	default:
		return defaultNoopAdapter
	}
}

// GetPayloadAdapterForModel returns the appropriate PayloadAdapter for the given model identifier.
func GetPayloadAdapterForModel(model string) PayloadAdapter {
	return GetPayloadAdapter(CategorizeModel(model))
}
