package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/100bench/OPORA/internal/domain"
)

const (
	maxJSONBody  = 8 << 20
	maxJSONDepth = 100
)

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return &domain.AppError{Code: domain.CodeUnsupportedMedia}
	}
	if charset, ok := params["charset"]; ok && !strings.EqualFold(charset, "utf-8") {
		return &domain.AppError{Code: domain.CodeUnsupportedMedia}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &domain.AppError{Code: domain.CodePayloadTooLarge, Cause: err}
		}
		return invalid("invalid JSON body", err)
	}
	if !utf8.Valid(body) {
		return invalid("invalid JSON body", nil)
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return invalid("invalid JSON body", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return invalid("invalid JSON body", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalid("body must contain one JSON value", err)
	}
	if err := validateRequiredJSON(body, dst); err != nil {
		return invalid("invalid JSON body", err)
	}
	return nil
}

func rejectDuplicateJSONKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := scanJSONValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanJSONValue(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nesting is too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for dec.More() {
			keyToken, keyErr := dec.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if _, exists := keys[key]; exists {
				return errors.New("duplicate object key")
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		end, endErr := dec.Token()
		if endErr != nil || end != json.Delim('}') {
			return errors.New("invalid object")
		}
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		end, endErr := dec.Token()
		if endErr != nil || end != json.Delim(']') {
			return errors.New("invalid array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

type rawObject map[string]json.RawMessage

func validateRequiredJSON(body []byte, dst any) error {
	var root rawObject
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return errors.New("top-level object required")
	}
	switch dst.(type) {
	case *createSessionRequest:
		if err := allowFields(root, "apps", "contacts"); err != nil {
			return err
		}
		if err := requireFields(root, "apps"); err != nil {
			return err
		}
		if err := validateObjectArray(root["apps"], validateAppObject); err != nil {
			return err
		}
		return validateOptionalObjectArray(root, "contacts", validateContactObject)
	case *domain.TurnRequest:
		return validateTurnObject(root)
	case *domain.ConfirmationRequest:
		if err := allowFields(root, "request_id", "action_id", "screen_version", "generation", "decision"); err != nil {
			return err
		}
		return requireFields(root, "request_id", "action_id", "screen_version", "generation", "decision")
	case *domain.ActionResultRequest:
		if err := allowFields(root, "request_id", "action_id", "status"); err != nil {
			return err
		}
		return requireFields(root, "request_id", "action_id", "status")
	case *domain.CancelRequest:
		if err := allowFields(root, "request_id"); err != nil {
			return err
		}
		return requireFields(root, "request_id")
	default:
		return nil
	}
}

func requireFields(obj rawObject, fields ...string) error {
	for _, field := range fields {
		raw, ok := obj[field]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("required field %q is missing", field)
		}
	}
	return nil
}

func validateObjectArray(raw json.RawMessage, validate func(rawObject) error) error {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil || items == nil {
		return errors.New("array field is invalid")
	}
	for _, item := range items {
		var obj rawObject
		if json.Unmarshal(item, &obj) != nil || obj == nil {
			return errors.New("array object is invalid")
		}
		if err := validate(obj); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalObjectArray(obj rawObject, field string, validate func(rawObject) error) error {
	raw, ok := obj[field]
	if !ok {
		return nil
	}
	return validateObjectArray(raw, validate)
}

func validateStringArrayIfPresent(obj rawObject, field string) error {
	raw, ok := obj[field]
	if !ok {
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return errors.New("array field is invalid")
	}
	return nil
}

func validateAppObject(obj rawObject) error {
	if err := allowFields(obj, "ref", "label", "aliases"); err != nil {
		return err
	}
	if err := requireFields(obj, "ref", "label"); err != nil {
		return err
	}
	return validateStringArrayIfPresent(obj, "aliases")
}

func validateContactObject(obj rawObject) error { return validateAppObject(obj) }

func validateTurnObject(root rawObject) error {
	if err := allowFields(root, "request_id", "text", "screen", "image"); err != nil {
		return err
	}
	if err := requireFields(root, "request_id", "text", "screen"); err != nil {
		return err
	}
	var screen rawObject
	if json.Unmarshal(root["screen"], &screen) != nil || screen == nil {
		return errors.New("screen object is invalid")
	}
	if err := allowFields(screen, "package", "version", "width", "height", "protected", "sensitive", "nodes"); err != nil {
		return err
	}
	if err := requireFields(screen, "package", "version", "width", "height", "nodes"); err != nil {
		return err
	}
	if err := rejectNullIfPresent(screen, "protected", "sensitive"); err != nil {
		return err
	}
	if err := validateObjectArray(screen["nodes"], validateNodeObject); err != nil {
		return err
	}
	if raw, ok := root["image"]; ok {
		var image rawObject
		if json.Unmarshal(raw, &image) != nil || image == nil {
			return errors.New("image object is invalid")
		}
		if err := allowFields(image, "media_type", "data_base64", "consent", "screen_version"); err != nil {
			return err
		}
		if err := requireFields(image, "media_type", "data_base64", "consent", "screen_version"); err != nil {
			return err
		}
	}
	return nil
}

func validateNodeObject(node rawObject) error {
	if err := allowFields(node, "id", "text", "bounds", "enabled", "visible", "actions", "sensitive"); err != nil {
		return err
	}
	if err := requireFields(node, "id", "bounds", "enabled", "visible"); err != nil {
		return err
	}
	if err := rejectNullIfPresent(node, "text", "sensitive"); err != nil {
		return err
	}
	var bounds rawObject
	if json.Unmarshal(node["bounds"], &bounds) != nil || bounds == nil {
		return errors.New("bounds object is invalid")
	}
	if err := allowFields(bounds, "left", "top", "right", "bottom"); err != nil {
		return err
	}
	if err := requireFields(bounds, "left", "top", "right", "bottom"); err != nil {
		return err
	}
	return validateStringArrayIfPresent(node, "actions")
}

func rejectNullIfPresent(obj rawObject, fields ...string) error {
	for _, field := range fields {
		if raw, ok := obj[field]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("field %q cannot be null", field)
		}
	}
	return nil
}

func allowFields(obj rawObject, fields ...string) error {
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
	}
	for field := range obj {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("field %q is not allowed", field)
		}
	}
	return nil
}
