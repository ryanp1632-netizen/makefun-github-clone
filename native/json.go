// JSON output with JavaScript's JSON.stringify conventions: ordered objects (O), integer-valued
// floats printed without a fraction, and only the escapes JSON.stringify itself writes.

package main

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// KV is one member of an ordered JSON object.
type KV struct {
	K string
	V any
}

// O is an ordered JSON object.
type O []KV

// Get returns the value of key k.
func (o O) Get(k string) (any, bool) {
	for _, kv := range o {
		if kv.K == k {
			return kv.V, true
		}
	}
	return nil, false
}

// Set replaces key k or appends it.
func (o O) Set(k string, v any) O {
	for i := range o {
		if o[i].K == k {
			o[i].V = v
			return o
		}
	}
	return append(o, KV{k, v})
}

// rawJSON is a pre-encoded JSON value.
type rawJSON []byte

const hexDigits = "0123456789abcdef"

func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < 0x80 {
			i++
			continue
		}
		if c < 0x80 {
			b = append(b, s[start:i]...)
			switch c {
			case '"':
				b = append(b, '\\', '"')
			case '\\':
				b = append(b, '\\', '\\')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			case '\b':
				b = append(b, '\\', 'b')
			case '\f':
				b = append(b, '\\', 'f')
			default:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, "�"...)
			i++
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

func jsNum(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		if f == 0 {
			return "0"
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	a := math.Abs(f)
	if a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	s = strings.Replace(s, "e-0", "e-", 1)
	s = strings.Replace(s, "e+0", "e+", 1)
	return s
}

func appendJSON(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...)
	case bool:
		if x {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case int:
		return strconv.AppendInt(b, int64(x), 10)
	case int64:
		return strconv.AppendInt(b, x, 10)
	case float64:
		return append(b, jsNum(x)...)
	case string:
		return appendJSONString(b, x)
	case *string:
		if x == nil {
			return append(b, "null"...)
		}
		return appendJSONString(b, *x)
	case rawJSON:
		return append(b, x...)
	case O:
		b = append(b, '{')
		first := true
		for _, kv := range x {
			if _, skip := kv.V.(undef); skip {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = appendJSONString(b, kv.K)
			b = append(b, ':')
			b = appendJSON(b, kv.V)
		}
		return append(b, '}')
	case []any:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSON(b, e)
		}
		return append(b, ']')
	case []O:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSON(b, e)
		}
		return append(b, ']')
	case []string:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, e)
		}
		return append(b, ']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, k)
			b = append(b, ':')
			b = appendJSON(b, x[k])
		}
		return append(b, '}')
	default:
		return append(b, "null"...)
	}
}

// undef marks an object member JSON.stringify would leave out (an undefined value).
type undef struct{}

func toJSON(v any) []byte { return appendJSON(make([]byte, 0, 256), v) }

// parseJSON parses a request body the way JSON.parse does (numbers as float64).
func parseJSON(data []byte) (any, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ------------------------------------------------------------------ JS-ish value helpers

func asStr(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asObj(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// jsString is String(v) for the values a parsed JSON body can hold (missing = undefined).
func jsString(v any, present bool) string {
	if !present {
		return "undefined"
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsNum(x)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				parts[i] = ""
			} else {
				parts[i] = jsString(e, true)
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	return ""
}

// jsonStringifyValue is JSON.stringify(v) for message text (undefined -> "undefined" is not JSON; callers handle it).
func jsonStringifyValue(v any, present bool) string {
	if !present {
		return "undefined"
	}
	return string(toJSON(v))
}

// ------------------------------------------------------------------ base64 like Node's lenient decoder

func lenientB64(s string) []byte {
	buf := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			buf = append(buf, c)
		case c == '-':
			buf = append(buf, '+')
		case c == '_':
			buf = append(buf, '/')
		case c == '=':
			i = len(s)
		}
	}
	if len(buf)%4 == 1 {
		buf = buf[:len(buf)-1]
	}
	out, err := base64.RawStdEncoding.DecodeString(string(buf))
	if err != nil {
		return nil
	}
	return out
}

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func stdB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
