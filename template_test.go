package main

import (
	"strings"
	"testing"
)

func TestRenderTitleDefault(t *testing.T) {
	p, _ := parseFilename("2026 Tornado Tumble Qualification Match 5.mp4")
	ctx := buildTemplateContext(p, nil, eventConfig{TitleTemplate: defaultTitleTemplate})
	got := renderTitle(defaultTitleTemplate, ctx)
	want := "2026 Tornado Tumble Qualification Match 5"
	if got != want {
		t.Errorf("title: %q want %q", got, want)
	}
}

func TestRenderTitlePlay(t *testing.T) {
	p, _ := parseFilename("2026 Tornado Tumble Qualification Match 5 Play 2.mp4")
	ctx := buildTemplateContext(p, nil, eventConfig{})
	got := renderTitle(defaultTitleTemplate, ctx)
	want := "2026 Tornado Tumble Qualification Match 5 Play 2"
	if got != want {
		t.Errorf("title: %q want %q", got, want)
	}
}

func TestRenderDescriptionWithAlliances(t *testing.T) {
	p, _ := parseFilename("2026 X Qualification Match 5.mp4")
	meta := &videoMeta{
		Alliances: map[string][]allianceTeam{
			"red": {
				{Number: 1234, Name: "A"},
				{Number: 2345, Name: "B"},
				{Number: 3456, Name: "C"},
			},
			"blue": {
				{Number: 4567, Name: "D"},
				{Number: 5678, Name: "E"},
				{Number: 6789, Name: "F"},
			},
		},
	}
	ctx := buildTemplateContext(p, meta, eventConfig{})
	ctx.Title = "TITLE"
	got := renderDescription(defaultDescriptionTemplate, ctx)
	if !strings.Contains(got, "TITLE") {
		t.Errorf("missing title: %q", got)
	}
	if !strings.Contains(got, "- 1234 A") {
		t.Errorf("missing red[0]: %q", got)
	}
	if !strings.Contains(got, "- 6789 F") {
		t.Errorf("missing blue[2]: %q", got)
	}
}

func TestRenderDescriptionDropsUndefinedLines(t *testing.T) {
	// No alliances supplied. All "- {red[N].number} {red[N].name}" lines
	// should be dropped, leaving just the title and the section headers
	// (which contain no placeholders) — but back-to-back empty sections
	// should be collapsed.
	p, _ := parseFilename("2026 X Qualification Match 5.mp4")
	ctx := buildTemplateContext(p, nil, eventConfig{})
	ctx.Title = "TITLE"
	got := renderDescription(defaultDescriptionTemplate, ctx)
	if strings.Contains(got, "{") {
		t.Errorf("description still has unresolved placeholders: %q", got)
	}
	if strings.Contains(got, "- ") {
		t.Errorf("alliance lines should have been dropped: %q", got)
	}
	if !strings.Contains(got, "TITLE") {
		t.Errorf("title still required: %q", got)
	}
}

func TestRenderDescriptionScore(t *testing.T) {
	p, _ := parseFilename("2026 X Qualification Match 5.mp4")

	// Score present: the Final Score line renders (0 is a valid score).
	ctx := buildTemplateContext(p, nil, eventConfig{})
	ctx.Title = "TITLE"
	ctx.RedScore, ctx.BlueScore, ctx.HasScore = 88, 0, true
	got := renderDescription(defaultDescriptionTemplate, ctx)
	if !strings.Contains(got, "Final Score: Red 88 - Blue 0") {
		t.Errorf("missing score line: %q", got)
	}

	// No score: the whole Final Score block drops, no stray placeholders.
	ctx2 := buildTemplateContext(p, nil, eventConfig{})
	ctx2.Title = "TITLE"
	got2 := renderDescription(defaultDescriptionTemplate, ctx2)
	if strings.Contains(got2, "Final Score") {
		t.Errorf("score block should drop with no score: %q", got2)
	}
	if strings.Contains(got2, "{") {
		t.Errorf("unresolved placeholder: %q", got2)
	}
}

func TestPartialAlliance(t *testing.T) {
	// Only two teams on red; third row should drop, others kept.
	tmpl := "{red[0].number}\n{red[1].number}\n{red[2].number}"
	ctx := &templateContext{
		Alliances: map[string][]allianceTeam{
			"red": {{Number: 1111}, {Number: 2222}},
		},
	}
	got := renderDescription(tmpl, ctx)
	if got != "1111\n2222" {
		t.Errorf("got %q", got)
	}
}

func TestEmptyNameRendersAsEmpty(t *testing.T) {
	// Team data present but no names: line stays, name renders empty,
	// trailing whitespace gets trimmed.
	tmpl := "- {red[0].number} {red[0].name}"
	ctx := &templateContext{
		Alliances: map[string][]allianceTeam{
			"red": {{Number: 1234, Name: ""}},
		},
	}
	got := renderDescription(tmpl, ctx)
	if got != "- 1234" {
		t.Errorf("got %q want %q", got, "- 1234")
	}
}
