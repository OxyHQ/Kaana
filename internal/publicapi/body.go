package publicapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf16"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// readBody reads at most limit bytes, refusing a larger body as
// request_too_large before anything else is read.
func readBody(w http.ResponseWriter, r *http.Request, requestID contract.RequestID, limit int64) ([]byte, *contract.PublicError) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, contract.NewPublicError(requestID, "request_too_large", fmt.Sprintf("the body exceeds %d bytes", limit))
		}
		return nil, invalid(requestID, "", "the body could not be read")
	}
	return body, nil
}

// requireJSON refuses a body that is not declared as JSON.
func requireJSON(r *http.Request, requestID contract.RequestID) *contract.PublicError {
	mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(strings.ToLower(mediaType)) != "application/json" {
		return invalid(requestID, "Content-Type", "the body must be application/json")
	}
	return nil
}

// decodeRequest reads a public request strictly: the contract refuses an
// unknown field on every public request rather than ignoring it, and a
// duplicated key or trailing JSON is a request that means two things.
func decodeRequest(body []byte, requestID contract.RequestID, destination any) *contract.PublicError {
	if err := rejectDuplicateKeys(body); err != nil {
		return invalid(requestID, "", err.Error())
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) && typeError.Field != "" {
			return invalid(requestID, typeError.Field, fmt.Sprintf("%s has the wrong type", typeError.Field))
		}
		if field, found := strings.CutPrefix(err.Error(), "json: unknown field "); found {
			field = strings.Trim(field, `"`)
			return invalid(requestID, field, fmt.Sprintf("%s is not a field of this request", field))
		}
		return invalid(requestID, "", "the body is not a JSON request of this endpoint")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalid(requestID, "", "the body has trailing JSON")
	}
	return nil
}

// rejectDuplicateKeys walks the JSON and refuses an object that names one key
// twice, which encoding/json would silently resolve to the last value.
func rejectDuplicateKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("the body is not JSON")
		}
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return errors.New("the body is not JSON")
				}
				name, _ := key.(string)
				if _, duplicate := seen[name]; duplicate {
					return fmt.Errorf("the key %q appears twice", name)
				}
				seen[name] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = decoder.Token()
		if err != nil {
			return errors.New("the body is not JSON")
		}
		return nil
	}
	return walk()
}

// utf16Length counts characters as the contract does: UTF-16 code units, what
// JavaScript's String.length reports.
func utf16Length(text string) int { return len(utf16.Encode([]rune(text))) }

// usageHeader renders units as Kaana-Usage: `unit=quantity` pairs.
func usageHeader(units []contract.UsageQuantity) string {
	pairs := make([]string, 0, len(units))
	for _, quantity := range units {
		pairs = append(pairs, fmt.Sprintf("%s=%d", quantity.Unit, quantity.Quantity))
	}
	return strings.Join(pairs, ",")
}

func nonNilUnits(units []contract.UsageQuantity) []contract.UsageQuantity {
	if units == nil {
		return []contract.UsageQuantity{}
	}
	return units
}
