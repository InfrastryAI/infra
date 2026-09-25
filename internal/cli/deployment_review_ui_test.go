package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/source"
	"github.com/charmbracelet/x/ansi"
)

func reviewUIFixture() deploymentReviewModel {
	return newDeploymentReviewModel(
		deploymentPlan{repository: source.Repository{Root: "/home/alex/Code/customer-api", Branch: "main"}, managed: true, initialCommit: true, initializeGit: true},
		api.Team{Ref: "acme", Name: "Acme"}, "/home/alex/Code/customer-api",
		deployOptions{name: "Customer API", remote: "origin"})
}

func reviewKey(model deploymentReviewModel, code rune) deploymentReviewModel {
	key := tea.KeyPressMsg{Code: code}
	if code >= ' ' && code <= '~' {
		key.Text = string(code)
	}
	updated, _ := model.Update(key)
	return updated.(deploymentReviewModel)
}

func TestDeploymentReviewUIRequiresExplicitApproval(t *testing.T) {
	for _, code := range []rune{tea.KeyEscape, 'q', 'c'} {
		model := reviewKey(reviewUIFixture(), code)
		if model.action != "cancel" {
			t.Fatalf("key %v unexpectedly approved deployment: %s", code, model.action)
		}
	}
	for _, code := range []rune{tea.KeyEnter, 'd'} {
		model := reviewKey(reviewUIFixture(), code)
		if model.action != "deploy" {
			t.Fatalf("key %v did not approve deployment", code)
		}
	}
	model := reviewKey(reviewUIFixture(), tea.KeyLeft)
	model = reviewKey(model, tea.KeyEnter)
	if model.action != "cancel" {
		t.Fatal("explicitly selected Cancel did not cancel")
	}
}

func TestDeploymentReviewUIEditsReturnForReview(t *testing.T) {
	model := reviewKey(reviewUIFixture(), 'e')
	if !model.editing || model.action != "" {
		t.Fatal("editing approved deployment")
	}
	model.fields[0].input.SetValue("Billing API")
	model.fields[1].input.SetValue("release")
	model.fields[2].input.SetValue("/work/billing")
	model.fields[3].input.SetValue("upstream")
	model = reviewKey(model, tea.KeyEnter)
	if model.action != "edit" || model.options.name != "Billing API" || model.options.remote != "upstream" || model.options.branch != "release" || model.directory != "/work/billing" {
		t.Fatalf("edits were not returned for a new review: %+v", model)
	}
	model = reviewKey(reviewUIFixture(), 'e')
	model.fields[0].input.SetValue("Discard this")
	model = reviewKey(model, tea.KeyEscape)
	if model.editing || model.action != "" || model.options.name != "Customer API" {
		t.Fatal("back did not discard pending edits")
	}
	model = reviewKey(model, 'e')
	for range 3 {
		model = reviewKey(model, tea.KeyTab)
	}
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "Upstream remote") || !strings.Contains(view, "4/4") || !strings.Contains(view, "╰") {
		t.Fatalf("editor did not scroll to the last field: %s", view)
	}
	model.fields[0].input.SetValue("x")
	model = reviewKey(model, tea.KeyEnter)
	if model.action != "" || model.validation == "" {
		t.Fatal("invalid edits were accepted")
	}
}

func TestDeploymentReviewUIFitsAndKeepsSourceDetails(t *testing.T) {
	model := reviewUIFixture()
	view := model.View()
	if view.AltScreen {
		t.Fatal("review should preserve terminal scrollback")
	}
	for _, detail := range []string{"NEW APPLICATION", "Welcome to Infrastry", "App name", "Customer API", "acme", "main", "/home/alex/Code/customer-api", "Initialize Git and commit files", "Create application", "Upload source and deploy", ".gitignore", "infrastry remote", "Deploy app", "Edit settings", "Cancel"} {
		if !strings.Contains(ansi.Strip(view.Content), detail) {
			t.Errorf("missing detail %q:\n%s", detail, ansi.Strip(view.Content))
		}
	}
	if !strings.Contains(ansi.Strip(view.Content), "╰") {
		t.Fatal("review card bottom was clipped")
	}
	for _, width := range []int{1, 24, 40, 60, 80, 120} {
		for _, height := range []int{1, 8, 16, 24, 40} {
			for _, editing := range []bool{false, true} {
				model := reviewUIFixture()
				model.options.name = strings.Repeat("界", 80) + "\x1b]52;c;payload\a"
				model.width, model.height = width, height
				if editing {
					model = reviewKey(model, 'e')
					for range 3 {
						model = reviewKey(model, tea.KeyTab)
					}
				}
				model.refresh()
				content := model.View().Content
				lines := strings.Split(content, "\n")
				if len(lines) > height || strings.Contains(content, "\x1b]52;") {
					t.Fatalf("unsafe or oversized view %dx%d: %q", width, height, content)
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > width {
						t.Fatalf("line exceeds width %d: %q", width, line)
					}
				}
			}
		}
	}
}

func TestDeploymentReviewUIProgramActions(t *testing.T) {
	for _, key := range []string{"d", "\x1b", "\x04"} {
		t.Run(fmt.Sprintf("key=%q", key), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			input, keyboard := io.Pipe()
			defer input.Close()
			defer keyboard.Close()
			go func() { _, _ = io.WriteString(keyboard, key) }()
			var output bytes.Buffer
			command := New(Dependencies{Stdin: input, Stderr: &output})
			fixture := reviewUIFixture()
			action, _, _, err := command.showDeploymentReview(ctx, fixture.plan, fixture.team, fixture.directory, fixture.options)
			if err != nil {
				t.Fatal(err)
			}
			want := "cancel"
			if key == "d" {
				want = "deploy"
			}
			if action != want {
				t.Fatalf("action=%q, want %q", action, want)
			}
		})
	}
}
