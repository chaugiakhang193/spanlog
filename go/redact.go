package spanlog

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// Redacted replaces the value of a sensitive key.
	Redacted = "[REDACTED]"
	// Truncated replaces a value below the nesting limit.
	Truncated = "[TRUNCATED]"
)

const defaultMaxDepth = 8

// lineSeparatorEscape is the escape of U+2028 and U+2029 without its last digit.
const lineSeparatorEscape = `\` + `u202`

// DefaultRedactKeys lists the keys that are always masked.
var DefaultRedactKeys = []string{
	"password",
	"token",
	"accessToken",
	"refreshToken",
	"apiKey",
	"secret",
	"authorization",
	"cookie",
	"email",
	"phone",
}

// RedactOptions configures Redact.
type RedactOptions struct {
	// Keys are masked in addition to DefaultRedactKeys.
	Keys []string
	// MaxDepth is the nesting level below which values are replaced by Truncated.
	// Nil or a negative number falls back to the default of 8.
	MaxDepth *int
}

// Redact returns a copy of value with every property whose key matches a
// sensitive key replaced by Redacted. Keys match without regard to case, at any
// depth, inside objects and arrays. The input is never modified. Only keys are
// matched. Sensitive text inside a string value is left as it is. A struct is
// read through its JSON form and its json tags apply. Redact returns an error
// for a value that encoding/json cannot encode, such as a channel or a cycle.
func Redact(value any, options RedactOptions) (any, error) {
	plain, err := toPlain(value)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(DefaultRedactKeys)+len(options.Keys))
	for _, key := range DefaultRedactKeys {
		keys[strings.ToLower(key)] = true
	}
	for _, key := range options.Keys {
		keys[strings.ToLower(key)] = true
	}
	maxDepth := defaultMaxDepth
	if options.MaxDepth != nil && *options.MaxDepth >= 0 {
		maxDepth = *options.MaxDepth
	}
	return redactValue(plain, keys, maxDepth), nil
}

func redactValue(value any, keys map[string]bool, depthLeft int) any {
	switch typed := value.(type) {
	case map[string]any:
		if depthLeft == 0 {
			return Truncated
		}
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			if keys[strings.ToLower(key)] {
				out[key] = Redacted
			} else {
				out[key] = redactValue(inner, keys, depthLeft-1)
			}
		}
		return out
	case []any:
		if depthLeft == 0 {
			return Truncated
		}
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = redactValue(inner, keys, depthLeft-1)
		}
		return out
	default:
		return value
	}
}

// toPlain reduces a value to maps, slices, strings, numbers, booleans and nil.
// json.Number keeps integers that a float64 cannot hold.
func toPlain(value any) (any, error) {
	encoded, err := encodeJSON(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var plain any
	if err := decoder.Decode(&plain); err != nil {
		return nil, err
	}
	return withoutNegativeZero(plain), nil
}

// withoutNegativeZero rewrites the -0 that encoding/json writes for a float64 as 0,
// the text of JSON.stringify in the Node.js package.
func withoutNegativeZero(plain any) any {
	switch typed := plain.(type) {
	case map[string]any:
		for key, inner := range typed {
			typed[key] = withoutNegativeZero(inner)
		}
	case []any:
		for i, inner := range typed {
			typed[i] = withoutNegativeZero(inner)
		}
	case json.Number:
		if typed == "-0" {
			return json.Number("0")
		}
	}
	return plain
}

// stableJSON encodes a value as compact JSON with object keys sorted at every
// level. A struct goes through toPlain because encoding/json writes its fields
// in declared order and sorts only map keys.
func stableJSON(value any) string {
	plain, err := toPlain(value)
	if err != nil {
		return "null"
	}
	var out bytes.Buffer
	if err := writeSorted(&out, plain); err != nil {
		return "null"
	}
	return out.String()
}

// writeSorted orders keys by UTF-16 code unit, the order of sort() in the Node.js
// package. encoding/json orders them by UTF-8 byte, which differs above U+FFFF.
func writeSorted(out *bytes.Buffer, plain any) error {
	switch typed := plain.(type) {
	case map[string]any:
		out.WriteByte('{')
		for i, key := range slices.SortedFunc(maps.Keys(typed), compareUTF16) {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeSorted(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := writeSorted(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for i, inner := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeSorted(out, inner); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	default:
		encoded, err := encodeJSON(plain)
		if err != nil {
			return err
		}
		out.Write(encoded)
	}
	return nil
}

func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

// encodeJSON writes the text that JSON.stringify writes in the Node.js package.
// SetEscapeHTML(false) leaves <, > and & alone. U+2028 and U+2029 stay escaped
// after it, and unescapeLineSeparators reverses them.
func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return unescapeLineSeparators(bytes.TrimRight(buffer.Bytes(), "\n")), nil
}

// unescapeLineSeparators copies a backslash pair whole. An escaped backslash in
// front of the letters u2028 stays text.
func unescapeLineSeparators(encoded []byte) []byte {
	out := make([]byte, 0, len(encoded))
	for i := 0; i < len(encoded); i++ {
		if encoded[i] != '\\' || i+1 == len(encoded) {
			out = append(out, encoded[i])
			continue
		}
		if raw := lineSeparatorAt(encoded[i:]); raw != 0 {
			out = utf8.AppendRune(out, raw)
			i += len(lineSeparatorEscape)
			continue
		}
		out = append(out, encoded[i], encoded[i+1])
		i++
	}
	return out
}

// lineSeparatorAt returns the separator whose escape starts text, or 0.
func lineSeparatorAt(text []byte) rune {
	digit := len(lineSeparatorEscape)
	if len(text) <= digit || !bytes.HasPrefix(text, []byte(lineSeparatorEscape)) {
		return 0
	}
	switch text[digit] {
	case '8':
		return 0x2028
	case '9':
		return 0x2029
	default:
		return 0
	}
}
