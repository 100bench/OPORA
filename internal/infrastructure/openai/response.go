package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/100bench/OPORA/internal/domain"
)

func parseResponseOutput(body []byte) (string, error) {
	if !utf8.Valid(body) || rejectDuplicateJSONKeys(body) != nil {
		return "", errors.New("invalid provider response")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return "", errors.New("invalid provider response")
	}
	rawStatus, statusOK := fields["status"]
	rawOutput, outputOK := fields["output"]
	var status string
	var output []json.RawMessage
	if !statusOK || !outputOK || json.Unmarshal(rawStatus, &status) != nil || json.Unmarshal(rawOutput, &output) != nil || status != "completed" || output == nil {
		return "", errors.New("invalid provider response")
	}
	if incompleteDetails, ok := fields["incomplete_details"]; ok && !bytes.Equal(bytes.TrimSpace(incompleteDetails), []byte("null")) {
		return "", errors.New("incomplete provider response")
	}
	messageCount := 0
	var outputText string
	for _, item := range output {
		var itemFields map[string]json.RawMessage
		if err := json.Unmarshal(item, &itemFields); err != nil || itemFields == nil {
			return "", errors.New("invalid provider output item")
		}
		var itemType string
		if rawType, ok := itemFields["type"]; !ok || json.Unmarshal(rawType, &itemType) != nil || itemType == "" {
			return "", errors.New("invalid provider output item")
		}
		switch itemType {
		case "reasoning":
			continue
		case "message":
			messageCount++
			if messageCount != 1 {
				return "", errors.New("ambiguous provider output")
			}
			text, err := parseMessage(item)
			if err != nil {
				return "", err
			}
			outputText = text
		default:
			return "", errors.New("unexpected provider output item")
		}
	}
	if messageCount != 1 || outputText == "" || len(outputText) > maxStructuredText {
		return "", errors.New("missing provider output text")
	}
	return outputText, nil
}

func parseMessage(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", errors.New("invalid assistant message")
	}
	var role string
	var content []json.RawMessage
	rawRole, roleOK := fields["role"]
	rawContent, contentOK := fields["content"]
	if !roleOK || !contentOK || json.Unmarshal(rawRole, &role) != nil || json.Unmarshal(rawContent, &content) != nil || role != "assistant" || content == nil {
		return "", errors.New("invalid assistant message")
	}
	if rawStatus, ok := fields["status"]; ok {
		var status string
		if json.Unmarshal(rawStatus, &status) != nil || status != "completed" {
			return "", errors.New("incomplete assistant message")
		}
	}
	if len(content) != 1 {
		return "", errors.New("ambiguous assistant content")
	}
	var contentFields map[string]json.RawMessage
	if err := json.Unmarshal(content[0], &contentFields); err != nil || contentFields == nil {
		return "", errors.New("refused or invalid assistant content")
	}
	var contentType, text string
	rawType, typeOK := contentFields["type"]
	rawText, textOK := contentFields["text"]
	if !typeOK || !textOK || json.Unmarshal(rawType, &contentType) != nil || json.Unmarshal(rawText, &text) != nil || contentType != "output_text" {
		return "", errors.New("refused or invalid assistant content")
	}
	return text, nil
}

type structuredReply struct {
	Kind             *domain.ReplyKind `json:"kind"`
	Text             *string           `json:"text"`
	HighlightNodeIDs *[]string         `json:"highlight_node_ids"`
	Action           json.RawMessage   `json:"action"`
}

func parseStructuredReply(body []byte, in domain.PlanInput) (domain.PlanOutput, error) {
	if len(body) > maxStructuredText || !utf8.Valid(body) || rejectDuplicateJSONKeys(body) != nil {
		return domain.PlanOutput{}, errors.New("invalid structured reply")
	}
	if err := requireExactObjectKeys(body, "kind", "text", "highlight_node_ids", "action"); err != nil {
		return domain.PlanOutput{}, errors.New("invalid structured reply")
	}
	var dto structuredReply
	if err := decodeOneJSON(body, &dto, true); err != nil || dto.Kind == nil || dto.Text == nil || dto.HighlightNodeIDs == nil || len(dto.Action) == 0 {
		return domain.PlanOutput{}, errors.New("invalid structured reply")
	}
	if !validReplyKind(*dto.Kind) || utf8.RuneCountInString(*dto.Text) < 1 || utf8.RuneCountInString(*dto.Text) > 4000 || len(*dto.HighlightNodeIDs) > 1 {
		return domain.PlanOutput{}, errors.New("invalid structured reply")
	}
	for _, id := range *dto.HighlightNodeIDs {
		if utf8.RuneCountInString(id) < 1 || utf8.RuneCountInString(id) > 128 {
			return domain.PlanOutput{}, errors.New("invalid highlight node")
		}
	}
	reply := domain.Reply{Kind: *dto.Kind, Text: *dto.Text, HighlightNodeIDs: *dto.HighlightNodeIDs}
	actionIsNull := bytes.Equal(bytes.TrimSpace(dto.Action), []byte("null"))
	if reply.Kind != domain.ReplyActionProposal {
		if !actionIsNull {
			return domain.PlanOutput{}, errors.New("unexpected action")
		}
		return domain.PlanOutput{Reply: reply}, nil
	}
	if actionIsNull {
		return domain.PlanOutput{}, errors.New("missing action")
	}
	action, err := parseAction(dto.Action, in)
	if err != nil {
		return domain.PlanOutput{}, err
	}
	reply.Action = action
	return domain.PlanOutput{Reply: reply}, nil
}

func parseAction(raw json.RawMessage, in domain.PlanInput) (*domain.ActionProposal, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("invalid action")
	}
	if _, ok := fields["kind"]; !ok {
		return nil, errors.New("invalid action")
	}
	var header struct {
		Kind *domain.ActionKind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &header); err != nil || header.Kind == nil {
		return nil, errors.New("invalid action")
	}
	switch *header.Kind {
	case domain.ActionOpenApp:
		if err := requireExactObjectKeys(raw, "kind", "app_ref", "display_text"); err != nil {
			return nil, errors.New("invalid open-app action")
		}
		var dto struct {
			Kind        *domain.ActionKind `json:"kind"`
			AppRef      *string            `json:"app_ref"`
			DisplayText *string            `json:"display_text"`
		}
		if err := decodeOneJSON(raw, &dto, true); err != nil || dto.Kind == nil || dto.AppRef == nil || dto.DisplayText == nil {
			return nil, errors.New("invalid open-app action")
		}
		if !validRef(*dto.AppRef) || !validDisplayText(*dto.DisplayText) {
			return nil, errors.New("invalid open-app action")
		}
		action := &domain.ActionProposal{Kind: *dto.Kind, AppRef: *dto.AppRef, DisplayText: *dto.DisplayText}
		for _, app := range in.Apps {
			if app.Ref == action.AppRef {
				action.AppLabel = app.Label
				break
			}
		}
		return action, nil
	case domain.ActionPrepareDialer:
		if err := requireExactObjectKeys(raw, "kind", "contact_ref", "display_text"); err != nil {
			return nil, errors.New("invalid dialer action")
		}
		var dto struct {
			Kind        *domain.ActionKind `json:"kind"`
			ContactRef  *string            `json:"contact_ref"`
			DisplayText *string            `json:"display_text"`
		}
		if err := decodeOneJSON(raw, &dto, true); err != nil || dto.Kind == nil || dto.ContactRef == nil || dto.DisplayText == nil {
			return nil, errors.New("invalid dialer action")
		}
		if !validRef(*dto.ContactRef) || !validDisplayText(*dto.DisplayText) {
			return nil, errors.New("invalid dialer action")
		}
		return &domain.ActionProposal{Kind: *dto.Kind, ContactRef: *dto.ContactRef, DisplayText: *dto.DisplayText}, nil
	default:
		return nil, errors.New("unsupported action")
	}
}

func requireExactObjectKeys(body []byte, expected ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil || len(object) != len(expected) {
		return errors.New("invalid object fields")
	}
	for _, field := range expected {
		if _, ok := object[field]; !ok {
			return errors.New("invalid object fields")
		}
	}
	return nil
}

func validReplyKind(kind domain.ReplyKind) bool {
	switch kind {
	case domain.ReplyExplain, domain.ReplyHighlight, domain.ReplyClarify, domain.ReplyActionProposal, domain.ReplyNeedImage, domain.ReplyRefuse:
		return true
	default:
		return false
	}
}

func validRef(ref string) bool {
	n := utf8.RuneCountInString(ref)
	return n >= 1 && n <= 128
}

func validDisplayText(text string) bool {
	n := utf8.RuneCountInString(text)
	return n >= 1 && n <= 1000
}

func decodeOneJSON(body []byte, dst any, disallowUnknown bool) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if disallowUnknown {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

const maxJSONDepth = 100

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
			foldedKey := strings.ToLower(key)
			if _, exists := keys[foldedKey]; exists {
				return errors.New("duplicate object key")
			}
			keys[foldedKey] = struct{}{}
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
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}
