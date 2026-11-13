package nativeadmission

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// canonicalManifest matches @oxy.so/protocol canonicalize for this manifest:
// UTF-16 object-key order, JSON.stringify string escaping, and integer numbers.
// Amounts never enter this format. In particular HTML and U+2028 stay literal.
func canonicalManifest(value any) ([]byte, error) {
	var out bytes.Buffer
	var write func(any) error
	write = func(value any) error {
		switch v := value.(type) {
		case nil:
			out.WriteString("null")
		case bool:
			out.WriteString(strconv.FormatBool(v))
		case string:
			if !utf8.ValidString(v) {
				return errors.New("invalid UTF-8 manifest string")
			}
			out.WriteByte('"')
			for _, r := range v {
				switch r {
				case '"', '\\':
					out.WriteByte('\\')
					out.WriteRune(r)
				case '\b':
					out.WriteString(`\b`)
				case '\f':
					out.WriteString(`\f`)
				case '\n':
					out.WriteString(`\n`)
				case '\r':
					out.WriteString(`\r`)
				case '\t':
					out.WriteString(`\t`)
				default:
					if r < 0x20 {
						out.WriteString(`\u00`)
						out.WriteByte("0123456789abcdef"[r>>4])
						out.WriteByte("0123456789abcdef"[r&15])
					} else {
						out.WriteRune(r)
					}
				}
			}
			out.WriteByte('"')
		case json.Number:
			n, err := strconv.ParseInt(string(v), 10, 64)
			if err != nil || n < 0 || n > 9007199254740991 {
				return errors.New("manifest number is not a safe unsigned integer")
			}
			out.WriteString(strconv.FormatInt(n, 10))
		case []any:
			out.WriteByte('[')
			for i, item := range v {
				if i > 0 {
					out.WriteByte(',')
				}
				if err := write(item); err != nil {
					return err
				}
			}
			out.WriteByte(']')
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			slices.SortFunc(keys, utf16Compare)
			out.WriteByte('{')
			for i, key := range keys {
				if i > 0 {
					out.WriteByte(',')
				}
				if err := write(key); err != nil {
					return err
				}
				out.WriteByte(':')
				if err := write(v[key]); err != nil {
					return err
				}
			}
			out.WriteByte('}')
		default:
			return errors.New("unsupported manifest value")
		}
		return nil
	}
	if err := write(value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func utf16Compare(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}
