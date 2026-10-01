package httpapi

import (
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// validateDecisionJSONUnicode refuses strings encoding/json would silently
// replace with U+FFFD. It inspects signed bytes, never rewrites them. Only the
// decisions endpoint uses this gate; unrelated inference families are unchanged.
func validateDecisionJSONUnicode(body []byte) error {
	if !utf8.Valid(body) {
		return errors.New("the decisions envelope contains invalid UTF-8")
	}
	if !json.Valid(body) {
		return errors.New("the decisions envelope is not valid JSON")
	}
	inString := false
	for i := 0; i < len(body); i++ {
		if body[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || body[i] != '\\' {
			continue
		}
		i++
		if body[i] != 'u' {
			continue
		}
		unit := jsonHexUnit(body[i+1 : i+5])
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return errors.New("the decisions envelope contains an unpaired Unicode surrogate")
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
			return errors.New("the decisions envelope contains an unpaired Unicode surrogate")
		}
		low := jsonHexUnit(body[i+3 : i+7])
		if low < 0xdc00 || low > 0xdfff {
			return errors.New("the decisions envelope contains an unpaired Unicode surrogate")
		}
		i += 6
	}
	return nil
}

// json.Valid has already checked that these four bytes are hexadecimal.
func jsonHexUnit(digits []byte) uint16 {
	var value uint16
	for _, digit := range digits {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value += uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += uint16(digit - 'a' + 10)
		default:
			value += uint16(digit - 'A' + 10)
		}
	}
	return value
}
