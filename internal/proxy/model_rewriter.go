package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unsafe"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

var (
	// ErrModelNotFound indicates that no root-level "model" property was found.
	ErrModelNotFound = errors.New("model key not found at root level")
	// ErrInvalidJSON indicates that the JSON payload is malformed or truncated.
	ErrInvalidJSON = errors.New("invalid JSON payload")
	// ErrInvalidModelValue indicates that the "model" property was found but is not a string.
	ErrInvalidModelValue = errors.New("model property value is not a valid string")
	// ErrEmptyTargetModel is returned when the target model parameter is empty.
	ErrEmptyTargetModel = errors.New("target model cannot be empty")

	keyModel     = []byte("model")
	prefixModels = []byte("models/")
)

// ModelCategory represents the top-level quota and provider tier for a model.
type ModelCategory int

const (
	// CategoryUnknown represents unrecognized model identifiers.
	CategoryUnknown ModelCategory = iota
	// CategoryClaudeGPT covers Anthropic Claude and OpenAI GPT third-party models.
	CategoryClaudeGPT
	// CategoryGemini covers Google Gemini and Gemma first-party models.
	CategoryGemini
)

// String returns a canonical string representation of the ModelCategory.
func (c ModelCategory) String() string {
	switch c {
	case CategoryClaudeGPT:
		return "claude_gpt"
	case CategoryGemini:
		return "gemini"
	default:
		return "unknown"
	}
}

// ModelSource identifies where the model was discovered in the request.
type ModelSource string

const (
	SourcePath  ModelSource = "path"
	SourceQuery ModelSource = "query"
	SourceJSON  ModelSource = "json"
	SourceNone  ModelSource = "none"
)

// NormalizeModelName strips whitespace and the "models/" resource prefix if present.
func NormalizeModelName(model string) string {
	return strings.TrimPrefix(strings.TrimSpace(model), "models/")
}

// CategorizeModel identifies the model category based on standard prefixes,
// substrings, and model family tokens. It performs 0 heap allocations.
func CategorizeModel(model string) ModelCategory {
	m := strings.TrimSpace(model)
	if m == "" {
		return CategoryUnknown
	}
	m = strings.TrimPrefix(m, "models/")

	// Fast stack-allocated ASCII lowercase (0 heap allocs)
	var buf [64]byte
	var lower string
	if len(m) <= len(buf) {
		hasUpper := false
		for i := 0; i < len(m); i++ {
			c := m[i]
			if c >= 'A' && c <= 'Z' {
				buf[i] = c + ('a' - 'A')
				hasUpper = true
			} else {
				buf[i] = c
			}
		}
		if hasUpper {
			lower = unsafe.String(&buf[0], len(m))
		} else {
			lower = m
		}
	} else {
		lower = strings.ToLower(m)
	}

	// 1. Claude & GPT Family Check (evaluated first to prevent "pro" collision)
	if strings.Contains(lower, "claude") ||
		strings.Contains(lower, "gpt") ||
		strings.Contains(lower, "sonnet") ||
		strings.Contains(lower, "opus") ||
		strings.Contains(lower, "haiku") ||
		strings.Contains(lower, "anthropic") ||
		strings.Contains(lower, "openai") ||
		strings.Contains(lower, "chatgpt") ||
		strings.Contains(lower, "3p") ||
		lower == "o1" || lower == "o3" || lower == "o4" ||
		strings.HasPrefix(lower, "o1-") || strings.HasPrefix(lower, "o3-") || strings.HasPrefix(lower, "o4-") ||
		strings.HasPrefix(lower, "o1_") || strings.HasPrefix(lower, "o3_") || strings.HasPrefix(lower, "o4_") {
		return CategoryClaudeGPT
	}

	// 2. Gemini Family Check
	if strings.Contains(lower, "gemini") ||
		strings.Contains(lower, "gemma") ||
		strings.Contains(lower, "flash") ||
		strings.Contains(lower, "pro") ||
		strings.Contains(lower, "ultra") {
		return CategoryGemini
	}

	return CategoryUnknown
}

// CategorizeModelWithDefault categorizes a model, returning defaultCategory if unknown.
func CategorizeModelWithDefault(model string, defaultCategory ModelCategory) ModelCategory {
	cat := CategorizeModel(model)
	if cat == CategoryUnknown {
		return defaultCategory
	}
	return cat
}

// ExtractModelFromPath extracts the model name from REST-style URL paths like:
// /v1internal/models/gemini-2.5-pro:streamGenerateContent or /models/claude-3-7-sonnet:generateContent.
// Zero heap allocations.
func ExtractModelFromPath(path string) string {
	const marker = "/models/"
	idx := strings.Index(path, marker)
	if idx == -1 {
		return ""
	}
	sub := path[idx+len(marker):]
	if len(sub) == 0 {
		return ""
	}
	end := len(sub)
	for i := 0; i < len(sub); i++ {
		c := sub[i]
		if c == ':' || c == '/' || c == '?' || c == '#' {
			end = i
			break
		}
	}
	return sub[:end]
}

// FindModelValueBounds scans a JSON byte slice for the root-level "model" property (braceDepth == 1, bracketDepth == 0).
// It returns the model value byte subslice, and value slice boundaries [startValIdx, endValIdx] inside the quotes.
// Zero heap allocations.
func FindModelValueBounds(body []byte) (modelBytes []byte, startValIdx, endValIdx int, err error) {
	n := len(body)
	if n < 8 {
		return nil, -1, -1, ErrModelNotFound
	}

	// Fast SIMD check: if "model" does not occur anywhere in body, exit early
	if !bytes.Contains(body, keyModel) {
		return nil, -1, -1, ErrModelNotFound
	}

	braceDepth := 0
	bracketDepth := 0
	i := 0

	for i < n {
		b := body[i]

		// Fast skip whitespace
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			i++
			continue
		}

		if b == '{' {
			braceDepth++
			i++
			continue
		}

		if b == '}' {
			braceDepth--
			i++
			if braceDepth < 0 {
				return nil, -1, -1, ErrInvalidJSON
			}
			if braceDepth == 0 {
				break
			}
			continue
		}

		if b == '[' {
			bracketDepth++
			i++
			continue
		}

		if b == ']' {
			bracketDepth--
			i++
			if bracketDepth < 0 {
				return nil, -1, -1, ErrInvalidJSON
			}
			continue
		}

		// String token
		if b == '"' {
			strStart := i + 1
			strEnd := -1
			j := strStart
			for j < n {
				qIdx := bytes.IndexByte(body[j:], '"')
				if qIdx == -1 {
					return nil, -1, -1, ErrInvalidJSON
				}
				cand := j + qIdx
				// Count consecutive preceding backslashes
				slashes := 0
				for k := cand - 1; k >= strStart && body[k] == '\\'; k-- {
					slashes++
				}
				if slashes%2 == 0 {
					// Unescaped quote terminates the string
					strEnd = cand
					break
				}
				j = cand + 1
			}

			if strEnd == -1 {
				return nil, -1, -1, ErrInvalidJSON
			}

			// Check if root-level key
			if braceDepth == 1 && bracketDepth == 0 {
				keyCandidate := body[strStart:strEnd]
				// Look ahead past whitespace for ':'
				k := strEnd + 1
				for k < n && (body[k] == ' ' || body[k] == '\t' || body[k] == '\r' || body[k] == '\n') {
					k++
				}
				if k < n && body[k] == ':' {
					// It is an object key at root level!
					if bytes.Equal(keyCandidate, keyModel) {
						// Found root "model" key! Scan for string value.
						k++ // skip ':'
						for k < n && (body[k] == ' ' || body[k] == '\t' || body[k] == '\r' || body[k] == '\n') {
							k++
						}
						if k < n && body[k] == '"' {
							valStart := k + 1
							valEnd := -1
							vj := valStart
							for vj < n {
								vqIdx := bytes.IndexByte(body[vj:], '"')
								if vqIdx == -1 {
									return nil, -1, -1, ErrInvalidJSON
								}
								vcand := vj + vqIdx
								slashes := 0
								for sk := vcand - 1; sk >= valStart && body[sk] == '\\'; sk-- {
									slashes++
								}
								if slashes%2 == 0 {
									valEnd = vcand
									break
								}
								vj = vcand + 1
							}
							if valEnd != -1 {
								return body[valStart:valEnd], valStart, valEnd, nil
							}
						}
						return nil, -1, -1, ErrInvalidModelValue
					}
					i = k + 1
					continue
				}
			}

			i = strEnd + 1
			continue
		}

		i++
	}

	return nil, -1, -1, ErrModelNotFound
}

// FindModelOffsets is an alias for FindModelValueBounds.
func FindModelOffsets(body []byte) (modelBytes []byte, startValIdx, endValIdx int, err error) {
	return FindModelValueBounds(body)
}

// ExtractModelFromJSON scans body for the root-level "model" property and returns its value as a string.
// Achieves zero heap allocations via unsafe.String referencing the underlying buffer slice.
func ExtractModelFromJSON(body []byte) (string, error) {
	match, _, _, err := FindModelValueBounds(body)
	if err != nil {
		return "", err
	}
	if len(match) == 0 {
		return "", nil
	}
	return unsafe.String(unsafe.SliceData(match), len(match)), nil
}

// RewriteModelInBody creates a new byte slice replacing the root-level model with targetModel.
// Guarantees immutability of the input body slice and performs exactly 1 slice allocation.
func RewriteModelInBody(body []byte, targetModel string) ([]byte, error) {
	if strings.TrimSpace(targetModel) == "" {
		return nil, ErrEmptyTargetModel
	}

	oldModel, startValIdx, endValIdx, err := FindModelValueBounds(body)
	if err != nil {
		return nil, err
	}

	hasPrefix := bytes.HasPrefix(oldModel, prefixModels)
	cleanTarget := strings.TrimPrefix(targetModel, "models/")

	prefixLen := 0
	if hasPrefix {
		prefixLen = len("models/")
	}

	oldLen := endValIdx - startValIdx
	newLen := prefixLen + len(cleanTarget)
	delta := newLen - oldLen
	newBody := make([]byte, len(body)+delta)

	// Segment 1: Prefix before model value
	copy(newBody[:startValIdx], body[:startValIdx])

	// Segment 2: Target model value (with models/ if incoming had it)
	if prefixLen > 0 {
		copy(newBody[startValIdx:], "models/")
	}
	copy(newBody[startValIdx+prefixLen:], cleanTarget)

	// Segment 3: Suffix after model value
	copy(newBody[startValIdx+newLen:], body[endValIdx:])

	return newBody, nil
}

// RewriteModelInPath returns a new URL path with the model component replaced by targetModel.
// Strips any redundant "models/" prefix from targetModel and preserves method suffixes like :streamGenerateContent.
func RewriteModelInPath(path string, targetModel string) string {
	if path == "" || targetModel == "" {
		return path
	}
	const marker = "/models/"
	idx := strings.Index(path, marker)
	if idx == -1 {
		return path
	}
	modelStart := idx + len(marker)
	remainder := path[modelStart:]
	modelEnd := len(path)
	for i := 0; i < len(remainder); i++ {
		c := remainder[i]
		if c == ':' || c == '/' || c == '?' || c == '#' {
			modelEnd = modelStart + i
			break
		}
	}
	cleanTarget := NormalizeModelName(targetModel)
	return path[:modelStart] + cleanTarget + path[modelEnd:]
}

// RewriteModelInQuery rewrites model=<model> parameter in a raw query string.
func RewriteModelInQuery(rawQuery string, targetModel string) string {
	if rawQuery == "" || targetModel == "" {
		return rawQuery
	}
	cleanTarget := NormalizeModelName(targetModel)
	const targetParam = "model="
	cur := 0
	for {
		idx := strings.Index(rawQuery[cur:], targetParam)
		if idx == -1 {
			return rawQuery
		}
		paramIdx := cur + idx
		if paramIdx == 0 || rawQuery[paramIdx-1] == '&' {
			valStart := paramIdx + len(targetParam)
			valEnd := strings.IndexByte(rawQuery[valStart:], '&')
			if valEnd == -1 {
				valEnd = len(rawQuery)
			} else {
				valEnd = valStart + valEnd
			}
			var sb strings.Builder
			sb.Grow(len(rawQuery) - (valEnd - valStart) + len(cleanTarget))
			sb.WriteString(rawQuery[:valStart])
			sb.WriteString(cleanTarget)
			sb.WriteString(rawQuery[valEnd:])
			return sb.String()
		}
		cur = paramIdx + 1
	}
}

// MatchesBucketCategory evaluates whether a stored quota bucket (by DisplayName and BucketID) belongs to category.
func MatchesBucketCategory(cat ModelCategory, displayName, bucketID string) bool {
	name := strings.ToLower(displayName)
	id := strings.ToLower(bucketID)

	switch cat {
	case CategoryClaudeGPT:
		return strings.Contains(name, "claude") ||
			strings.Contains(name, "gpt") ||
			strings.Contains(name, "3p") ||
			strings.Contains(id, "claude") ||
			strings.Contains(id, "gpt") ||
			strings.Contains(id, "-3p-") ||
			strings.HasPrefix(id, "3p-") ||
			strings.Contains(id, "3p_") ||
			strings.Contains(id, "_3p") ||
			strings.Contains(id, "-3p")

	case CategoryGemini:
		return strings.Contains(name, "gemini") ||
			strings.Contains(id, "gemini")

	default:
		return false
	}
}

// MatchesBucket evaluates whether a domain.QuotaBucket matches cat.
func MatchesBucket(bucket *domain.QuotaBucket, cat ModelCategory) bool {
	if bucket == nil {
		return false
	}
	return MatchesBucketCategory(cat, bucket.DisplayName, bucket.BucketID)
}

// ApplyRewrittenBody updates req with newBody and newPath, synchronizing ContentLength,
// wire Content-Length header, and GetBody closure.
func ApplyRewrittenBody(req *http.Request, newBody []byte, newPath string) {
	if req == nil {
		return
	}

	if newPath != "" && req.URL != nil {
		req.URL.Path = newPath
		if req.URL.RawPath != "" {
			req.URL.RawPath = newPath
		}
	}

	bodyLen := len(newBody)
	if bodyLen == 0 {
		req.Body = http.NoBody
		req.ContentLength = 0
		req.Header.Del("Content-Length")
		req.GetBody = func() (io.ReadCloser, error) {
			return http.NoBody, nil
		}
		return
	}

	req.Body = io.NopCloser(bytes.NewReader(newBody))
	req.ContentLength = int64(bodyLen)
	req.Header.Set("Content-Length", strconv.Itoa(bodyLen))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
}

// SynchronizeRequest updates req with rewritten body, path, query, and synchronizes
// all transport headers and body readers.
func SynchronizeRequest(req *http.Request, newBody []byte, newPath string, newQuery string) {
	if req == nil {
		return
	}
	if newQuery != "" && req.URL != nil {
		req.URL.RawQuery = newQuery
	}
	ApplyRewrittenBody(req, newBody, newPath)
}

// ExtractModelFromRequest extracts the model identifier and category from an HTTP request.
// Inspects URL path first, URL query second, and JSON body third.
func ExtractModelFromRequest(r *http.Request, body []byte) (model string, category ModelCategory, source ModelSource) {
	if r == nil {
		return "", CategoryUnknown, SourceNone
	}
	if r.URL != nil {
		if pModel := ExtractModelFromPath(r.URL.Path); pModel != "" {
			return pModel, CategorizeModel(pModel), SourcePath
		}
		if qModel := r.URL.Query().Get("model"); qModel != "" {
			return qModel, CategorizeModel(qModel), SourceQuery
		}
	}
	if len(body) > 0 {
		if jModel, err := ExtractModelFromJSON(body); err == nil && jModel != "" {
			return jModel, CategorizeModel(jModel), SourceJSON
		}
	}
	return "", CategoryUnknown, SourceNone
}
