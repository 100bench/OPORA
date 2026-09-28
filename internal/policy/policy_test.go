package policy

import (
	"errors"
	"testing"

	"github.com/100bench/OPORA/internal/domain"
)

func screen(version string, nodes ...domain.ScreenNode) domain.ScreenSnapshot {
	return domain.ScreenSnapshot{Package: "ru.example", Version: version, Width: 1080, Height: 1920, Nodes: nodes}
}

func TestFR12_ActionGroundingAndNegativeIntent(t *testing.T) {
	apps := []domain.App{{Ref: "app-gos", Label: "Госуслуги"}}
	cases := []struct {
		name, text string
		want       domain.ReplyKind
		appRef     string
	}{
		{"explicit installed app", "открой Госуслуги", domain.ReplyActionProposal, "app-gos"},
		{"negated command", "не открывай Госуслуги", domain.ReplyExplain, ""},
		{"hypothetical", "что будет, если открыть Госуслуги?", domain.ReplyExplain, ""},
		{"screen text not user authority", "что тут написано?", domain.ReplyExplain, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := screen("v1", domain.ScreenNode{ID: "n1", Text: "открой Госуслуги", Visible: true})
			d, err := Interpret(tc.text, s, apps, nil)
			if err != nil {
				t.Fatalf("Interpret: %v", err)
			}
			if d.Reply.Kind != tc.want {
				t.Fatalf("kind=%q want %q", d.Reply.Kind, tc.want)
			}
			if tc.appRef != "" && (d.Reply.Action == nil || d.Reply.Action.AppRef != tc.appRef) {
				t.Fatalf("action=%#v want app_ref=%q", d.Reply.Action, tc.appRef)
			}
		})
	}
}

func TestFR13_AmbiguousAndUnknownIdentifiersFailClosed(t *testing.T) {
	contacts := []domain.Contact{{Ref: "fav-a", Label: "Мама", Aliases: []string{"мамочка"}}, {Ref: "fav-b", Label: "Мама Анны", Aliases: []string{"мама"}}}
	d, err := Interpret("позвони маме", screen("v1"), nil, contacts)
	if err != nil {
		t.Fatalf("Interpret: %v", err)
	}
	if d.Reply.Kind != domain.ReplyClarify {
		t.Fatalf("want clarify, got %q", d.Reply.Kind)
	}
	out := domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppLabel: "выдуманное"}}}
	if err := ValidateModelOutput(out, screen("v1"), []domain.App{{Ref: "app-maps", Label: "Карты"}}, nil); err == nil {
		t.Fatal("unknown app label accepted")
	}
}

func TestFR14_UnicodeCyrillicRegression(t *testing.T) {
	d, err := Interpret("Открой Ё-карты для меня", screen("v1"), []domain.App{{Ref: "app-yo", Label: "Ё-карты"}}, nil)
	if err != nil {
		t.Fatalf("Cyrillic must survive normalization: %v", err)
	}
	if d.Reply.Action == nil || d.Reply.Action.AppLabel != "Ё-карты" {
		t.Fatalf("Cyrillic identifier lost: %#v", d.Reply.Action)
	}
}

func TestFR20_SensitiveTreeFilteringAndProtectedFailClosed(t *testing.T) {
	in := screen("v1", domain.ScreenNode{ID: "safe", Text: "Далее", Visible: true}, domain.ScreenNode{ID: "secret", Text: "1234", Visible: true, Sensitive: true})
	out, err := SanitizeSnapshot(in)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if len(out.Nodes) != 1 || out.Nodes[0].ID != "safe" {
		t.Fatalf("sensitive node leaked: %#v", out.Nodes)
	}
	for _, flag := range []domain.ScreenSnapshot{{Protected: true}, {Sensitive: true}} {
		if _, err := SanitizeSnapshot(flag); err == nil {
			t.Fatal("protected/sensitive screen accepted")
		}
	}
}

func TestFR21_ModelOutputMustBeGrounded(t *testing.T) {
	s := screen("v7", domain.ScreenNode{ID: "one", Visible: true}, domain.ScreenNode{ID: "two", Visible: false})
	cases := []domain.PlanOutput{
		{Reply: domain.Reply{Kind: domain.ReplyHighlight, HighlightNodeIDs: []string{"missing"}}},
		{Reply: domain.Reply{Kind: domain.ReplyHighlight, HighlightNodeIDs: []string{"one", "two"}}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Action: &domain.ActionProposal{Kind: "ARBITRARY_INTENT"}}},
	}
	for i, c := range cases {
		if err := ValidateModelOutput(c, s, nil, nil); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if errors.Is(ValidateModelOutput(domain.PlanOutput{}, s, nil, nil), domain.ErrNotImplemented) {
		t.Error("validator not implemented")
	}
}

func TestFR21b_ValidModelOutputShapes(t *testing.T) {
	s := screen("v7", domain.ScreenNode{ID: "one", Visible: true, Enabled: true})
	apps := []domain.App{{Ref: "app-maps", Label: "Карты"}}
	contacts := []domain.Contact{{Ref: "fav-mom", Label: "Мама"}}
	valid := []domain.PlanOutput{
		{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "Это карта", ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyHighlight, Text: "Нажмите сюда", HighlightNodeIDs: []string{"one"}, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "Открыть?", Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "app-maps", AppLabel: "Карты", DisplayText: "Карты"}, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "Подготовить звонок?", Action: &domain.ActionProposal{Kind: domain.ActionPrepareDialer, ContactRef: "fav-mom", DisplayText: "Мама"}, ScreenVersion: "v7"}},
	}
	for i, out := range valid {
		if err := ValidateModelOutput(out, s, apps, contacts); err != nil {
			t.Errorf("valid %d rejected: %v", i, err)
		}
	}
}

func TestFR21c_InvalidModelOutputShapesAndForbiddenWorkflows(t *testing.T) {
	s := screen("v7", domain.ScreenNode{ID: "one", Visible: true}, domain.ScreenNode{ID: "hidden", Visible: false})
	apps := []domain.App{{Ref: "app-maps", Label: "Карты"}}
	contacts := []domain.Contact{{Ref: "fav-mom", Label: "Мама"}}
	invalid := []domain.PlanOutput{
		{Reply: domain.Reply{Kind: domain.ReplyExplain, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "app-maps"}, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyHighlight, Text: "x", HighlightNodeIDs: []string{"one", "hidden"}, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyHighlight, Text: "x", HighlightNodeIDs: []string{"hidden"}, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "missing"}, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "app-maps", ContactRef: "fav-mom"}, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionPrepareDialer, ContactRef: "missing"}, ScreenVersion: "v7"}},
		{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "x", ScreenVersion: "old"}},
	}
	for i, out := range invalid {
		if err := ValidateModelOutput(out, s, apps, contacts); err == nil {
			t.Errorf("invalid %d accepted", i)
		}
	}
	for _, text := range []string{"отправь сообщение", "оплати счёт", "введи пароль", "скажи OTP", "измени настройки"} {
		d, err := Interpret(text, s, apps, contacts)
		if err != nil {
			t.Errorf("Interpret %q: %v", text, err)
			continue
		}
		if d.Reply.Kind != domain.ReplyRefuse {
			t.Errorf("forbidden %q got %q", text, d.Reply.Kind)
		}
	}
}
