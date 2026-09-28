package policy

import (
	"strings"
	"testing"

	"github.com/100bench/OPORA/internal/domain"
)

func TestPolicyAdditionalInterpretationAndInflections(t *testing.T) {
	s := screen("v", domain.ScreenNode{ID: "route", Text: "Маршрут", Visible: true})
	apps := []domain.App{{Ref: "yo", Label: "Ё-карты", Aliases: []string{"карты"}}}
	contacts := []domain.Contact{{Ref: "grandson", Label: "Внук"}, {Ref: "maria", Label: "Мария"}, {Ref: "igor", Label: "Игорь"}, {Ref: "sergey", Label: "Сергей"}}
	cases := []struct {
		text string
		kind domain.ReplyKind
		ref  string
	}{
		{"", "", ""},
		{"запусти карты пожалуйста", domain.ReplyActionProposal, "yo"},
		{"позвони внуку", domain.ReplyActionProposal, "grandson"},
		{"позвони марие", domain.ReplyActionProposal, "maria"},
		{"позвони игорю", domain.ReplyActionProposal, "igor"},
		{"позвони сергею", domain.ReplyActionProposal, "sergey"},
		{"покажи кнопку маршрута", domain.ReplyHighlight, ""},
		{"покажи неизвестное", domain.ReplyClarify, ""},
		{"отправь сообщение", domain.ReplyRefuse, ""},
		{"можно ли позвонить внуку", domain.ReplyExplain, ""},
	}
	for _, tc := range cases {
		d, err := Interpret(tc.text, s, apps, contacts)
		if tc.text == "" {
			if err == nil {
				t.Fatal("empty text accepted")
			}
			continue
		}
		if err != nil || d.Reply.Kind != tc.kind {
			t.Fatalf("%q: kind=%q err=%v", tc.text, d.Reply.Kind, err)
		}
		if tc.ref != "" && d.Reply.Action != nil && d.Reply.Action.AppRef != tc.ref && d.Reply.Action.ContactRef != tc.ref {
			t.Fatalf("%q: action=%#v", tc.text, d.Reply.Action)
		}
	}

	ambiguous := []domain.App{{Ref: "a", Label: "Карты"}, {Ref: "b", Label: "Карты"}}
	d, err := Interpret("открой карты", s, ambiguous, nil)
	if err != nil || d.Reply.Kind != domain.ReplyClarify {
		t.Fatalf("ambiguous apps: %#v %v", d, err)
	}

	spaced := normalize("  ЁЖ\t\n ИВА  ")
	if spaced != "ёж ива" {
		t.Fatalf("normalization=%q", spaced)
	}
	if got := russianForms(""); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty forms=%v", got)
	}
}

func TestPolicyAdditionalModelShapes(t *testing.T) {
	s := screen("v", domain.ScreenNode{ID: "visible", Visible: true}, domain.ScreenNode{ID: "secret", Visible: true, Sensitive: true})
	apps := []domain.App{{Ref: "app", Label: "Карты"}}
	contacts := []domain.Contact{{Ref: "contact", Label: "Внук"}}
	valid := []domain.PlanOutput{
		{Reply: domain.Reply{Kind: domain.ReplyClarify, Text: "x"}},
		{Reply: domain.Reply{Kind: domain.ReplyNeedImage, Text: "x"}},
		{Reply: domain.Reply{Kind: domain.ReplyRefuse, Text: "x"}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "app", DisplayText: "x"}}},
	}
	for _, out := range valid {
		if err := ValidateModelOutput(out, s, apps, contacts); err != nil {
			t.Fatalf("valid output rejected: %#v: %v", out, err)
		}
	}
	invalid := []domain.PlanOutput{
		{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "x", Generation: 1}},
		{Reply: domain.Reply{Kind: domain.ReplyClarify, Text: "x", HighlightNodeIDs: []string{"visible"}}},
		{Reply: domain.Reply{Kind: domain.ReplyHighlight, Text: "x", HighlightNodeIDs: []string{"secret"}}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x"}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", HighlightNodeIDs: []string{"visible"}, Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "app", DisplayText: "x"}}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{ActionID: "provider", Kind: domain.ActionOpenApp, AppRef: "app", DisplayText: "x"}}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "app", AppLabel: "wrong", DisplayText: "x"}}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionPrepareDialer, AppLabel: "bad", ContactRef: "contact", DisplayText: "x"}}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionPrepareDialer, ContactRef: "unknown", DisplayText: "x"}}},
		{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "x", Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "app", DisplayText: strings.Repeat("x", 1001)}}},
	}
	for i, out := range invalid {
		if err := ValidateModelOutput(out, s, apps, contacts); err == nil {
			t.Errorf("invalid output %d accepted", i)
		}
	}
}
