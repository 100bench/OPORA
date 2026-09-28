// Package policy enforces deterministic action authority and output grounding.
package policy

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/100bench/OPORA/internal/domain"
)

// Decision is a deterministic safety decision. Final replies must not be
// replaced by a provider; non-final explain replies may be enriched by it.
type Decision struct {
	Reply domain.Reply
	Final bool
}

// Interpret recognizes conservative explicit commands and local safety decisions.
func Interpret(text string, screen domain.ScreenSnapshot, apps []domain.App, contacts []domain.Contact) (Decision, error) {
	normalized := normalize(text)
	base := domain.Reply{Kind: domain.ReplyExplain, Text: "Объясняю содержимое экрана.", ScreenVersion: screen.Version}
	if normalized == "" {
		return Decision{}, invalid("empty request")
	}
	if forbidden(normalized) {
		base.Kind = domain.ReplyRefuse
		base.Text = "Этот запрос нельзя выполнить безопасно."
		return Decision{Reply: base, Final: true}, nil
	}
	if negativeOrHypothetical(normalized) {
		base.Text = "Действие не будет выполнено."
		return Decision{Reply: base, Final: true}, nil
	}
	if target, ok := commandTarget(normalized, []string{"открой", "запусти"}); ok {
		matches := matchApps(target, apps)
		if len(matches) != 1 {
			return clarify(screen.Version, "Уточните, какое приложение открыть."), nil
		}
		a := matches[0]
		base.Kind = domain.ReplyActionProposal
		base.Text = "Подтвердите открытие приложения."
		base.Action = &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: a.Ref, AppLabel: a.Label, DisplayText: a.Label}
		return Decision{Reply: base, Final: true}, nil
	}
	if target, ok := commandTarget(normalized, []string{"позвони", "набери"}); ok {
		matches := matchContacts(target, contacts)
		if len(matches) != 1 {
			return clarify(screen.Version, "Уточните, для какого контакта подготовить номер."), nil
		}
		c := matches[0]
		base.Kind = domain.ReplyActionProposal
		base.Text = "Подтвердите подготовку номера в телефоне."
		base.Action = &domain.ActionProposal{Kind: domain.ActionPrepareDialer, ContactRef: c.Ref, DisplayText: c.Label}
		return Decision{Reply: base, Final: true}, nil
	}
	if target, ok := commandTarget(normalized, []string{"покажи"}); ok {
		target = strings.TrimSpace(strings.TrimPrefix(target, "кнопку "))
		var matches []string
		for _, n := range screen.Nodes {
			if n.Visible && !n.Sensitive {
				for _, form := range russianForms(normalize(n.Text)) {
					if targetMatches(target, form) {
						matches = append(matches, n.ID)
						break
					}
				}
			}
		}
		if len(matches) != 1 {
			return clarify(screen.Version, "Не удалось однозначно найти элемент на экране."), nil
		}
		base.Kind = domain.ReplyHighlight
		base.Text = "Нужный элемент выделен."
		base.HighlightNodeIDs = []string{matches[0]}
		return Decision{Reply: base, Final: true}, nil
	}
	return Decision{Reply: base}, nil
}

// SanitizeSnapshot rejects protected screens and removes sensitive nodes.
func SanitizeSnapshot(in domain.ScreenSnapshot) (domain.ScreenSnapshot, error) {
	if in.Protected || in.Sensitive {
		return domain.ScreenSnapshot{}, unsafe("protected or sensitive screen")
	}
	out := in
	out.Nodes = make([]domain.ScreenNode, 0, len(in.Nodes))
	for _, n := range in.Nodes {
		if n.Sensitive {
			continue
		}
		copyNode := n
		copyNode.Actions = append([]string(nil), n.Actions...)
		out.Nodes = append(out.Nodes, copyNode)
	}
	return out, nil
}

// ValidateModelOutput grounds an untrusted structured planner response.
func ValidateModelOutput(out domain.PlanOutput, screen domain.ScreenSnapshot, apps []domain.App, contacts []domain.Contact) error {
	r := out.Reply
	if !validText(r.Text, 1, 4000) {
		return invalid("invalid model reply text")
	}
	if r.Generation != 0 {
		return invalid("provider supplied server-owned generation")
	}
	if r.ScreenVersion != "" && r.ScreenVersion != screen.Version {
		return invalid("model reply has stale screen version")
	}
	switch r.Kind {
	case domain.ReplyExplain, domain.ReplyClarify, domain.ReplyNeedImage, domain.ReplyRefuse:
		if r.Action != nil || len(r.HighlightNodeIDs) != 0 {
			return invalid("invalid non-action reply shape")
		}
	case domain.ReplyHighlight:
		if r.Action != nil || len(r.HighlightNodeIDs) != 1 {
			return invalid("invalid highlight reply shape")
		}
		id := r.HighlightNodeIDs[0]
		found := false
		for _, n := range screen.Nodes {
			if n.ID == id && n.Visible && !n.Sensitive {
				found = true
				break
			}
		}
		if !found {
			return unsafe("highlight is not grounded")
		}
	case domain.ReplyActionProposal:
		if r.Action == nil || len(r.HighlightNodeIDs) != 0 {
			return invalid("invalid action reply shape")
		}
		if err := validateAction(*r.Action, apps, contacts); err != nil {
			return err
		}
	default:
		return invalid("unknown model reply kind")
	}
	return nil
}

func validateAction(a domain.ActionProposal, apps []domain.App, contacts []domain.Contact) error {
	if a.ActionID != "" {
		return invalid("provider supplied server-owned action id")
	}
	if !validText(a.DisplayText, 1, 1000) || !validText(a.AppLabel, 0, 128) || !validText(a.AppRef, 0, 128) || !validText(a.ContactRef, 0, 128) {
		return invalid("invalid action fields")
	}
	switch a.Kind {
	case domain.ActionOpenApp:
		if a.AppRef == "" || a.ContactRef != "" {
			return invalid("invalid OPEN_APP target")
		}
		for _, configured := range apps {
			if configured.Ref == a.AppRef {
				if a.AppLabel != "" && a.AppLabel != configured.Label {
					return unsafe("app label does not match configured ref")
				}
				return nil
			}
		}
		return unsafe("unknown app ref")
	case domain.ActionPrepareDialer:
		if a.ContactRef == "" || a.AppRef != "" || a.AppLabel != "" {
			return invalid("invalid PREPARE_DIALER target")
		}
		for _, configured := range contacts {
			if configured.Ref == a.ContactRef {
				return nil
			}
		}
		return unsafe("unknown contact ref")
	default:
		return unsafe("unsupported action kind")
	}
}

func clarify(version, text string) Decision {
	return Decision{Reply: domain.Reply{Kind: domain.ReplyClarify, Text: text, ScreenVersion: version}, Final: true}
}

func commandTarget(text string, verbs []string) (string, bool) {
	for _, verb := range verbs {
		prefix := verb + " "
		if strings.HasPrefix(text, prefix) {
			target := strings.TrimSpace(strings.Trim(text[len(prefix):], " .,!?:;"))
			return target, target != ""
		}
	}
	return "", false
}

func matchApps(target string, apps []domain.App) []domain.App {
	result := make([]domain.App, 0, 1)
	for _, app := range apps {
		if namedTarget(target, app.Label, app.Aliases) {
			result = append(result, app)
		}
	}
	return result
}

func matchContacts(target string, contacts []domain.Contact) []domain.Contact {
	result := make([]domain.Contact, 0, 1)
	for _, contact := range contacts {
		if namedTarget(target, contact.Label, contact.Aliases) {
			result = append(result, contact)
		}
	}
	return result
}

func namedTarget(target, label string, aliases []string) bool {
	for _, name := range append([]string{label}, aliases...) {
		for _, form := range russianForms(normalize(name)) {
			if targetMatches(target, form) {
				return true
			}
		}
	}
	return false
}

func targetMatches(target, name string) bool {
	if target == name {
		return true
	}
	for _, suffix := range []string{" пожалуйста", " для меня"} {
		if target == name+suffix {
			return true
		}
	}
	return false
}

func russianForms(name string) []string {
	forms := []string{name}
	words := strings.Fields(name)
	if len(words) == 0 {
		return forms
	}
	last := words[len(words)-1]
	prefix := strings.TrimSuffix(name, last)
	if strings.HasSuffix(last, "а") {
		stem := strings.TrimSuffix(last, "а")
		forms = append(forms, prefix+stem+"е", prefix+stem+"у")
	} else if strings.HasSuffix(last, "я") {
		stem := strings.TrimSuffix(last, "я")
		forms = append(forms, prefix+stem+"е", prefix+stem+"ю")
	} else if strings.HasSuffix(last, "й") {
		stem := strings.TrimSuffix(last, "й")
		forms = append(forms, prefix+stem+"я", prefix+stem+"ю")
	} else if strings.HasSuffix(last, "ь") {
		stem := strings.TrimSuffix(last, "ь")
		forms = append(forms, prefix+stem+"я", prefix+stem+"ю", prefix+stem+"и")
	} else if endsWithCyrillicConsonant(last) {
		forms = append(forms, prefix+last+"а", prefix+last+"у")
	}
	return forms
}

func endsWithCyrillicConsonant(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return unicode.In(r, unicode.Cyrillic) && !strings.ContainsRune("аеёиоуыэюяйьъ", r)
}

func forbidden(text string) bool {
	phrases := []string{"отправь сообщение", "оплати", "платеж", "введи пароль", "одноразовый код", "otp", "скажи код", "измени настройки", "vpn", "proxy", "прокси"}
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func negativeOrHypothetical(text string) bool {
	if strings.Contains(text, "что будет") || strings.Contains(text, "если ") || strings.HasPrefix(text, "можно ли ") {
		return true
	}
	words := strings.Fields(text)
	for i, word := range words {
		if word == "не" && i+1 < len(words) {
			next := strings.Trim(words[i+1], " .,!?:;")
			if strings.HasPrefix(next, "откр") || strings.HasPrefix(next, "запуст") || strings.HasPrefix(next, "позвон") || strings.HasPrefix(next, "набер") {
				return true
			}
		}
	}
	return false
}

func normalize(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.TrimSpace(s) {
		r = unicode.ToLower(r)
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		b.WriteRune(r)
		space = false
	}
	return strings.TrimSpace(b.String())
}

func validText(s string, minimum, maximum int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) >= minimum && utf8.RuneCountInString(s) <= maximum
}

func invalid(message string) error {
	return &domain.AppError{Code: domain.CodeInvalidArgument, Message: message}
}

func unsafe(message string) error {
	return &domain.AppError{Code: domain.CodeUnsafe, Message: message}
}
