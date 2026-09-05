package domain

// ModelInfo represents an AI model available in Google Antigravity 2.0.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category"` // "gemini" or "claude_gpt"
	Recommended bool   `json:"recommended"`
}
