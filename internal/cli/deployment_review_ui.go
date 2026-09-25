package cli

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/InfrastryAI/infra/internal/api"
	"github.com/charmbracelet/x/ansi"
)

func (command *CLI) deploymentReviewUIEnabled(options deployOptions) bool {
	return !options.yes && !options.json && command.canPrompt() &&
		outputIsTerminal(command.dependencies.Stdout) && outputIsTerminal(command.dependencies.Stderr) &&
		!command.noColor && command.dependencies.Getenv("NO_COLOR") == "" &&
		command.dependencies.Getenv("INFRASTRY_ACCESSIBLE") == "" &&
		command.dependencies.Getenv("CI") == "" && command.dependencies.Getenv("TERM") != "dumb"
}

func (command *CLI) showDeploymentReview(ctx context.Context, plan deploymentPlan, team api.Team, directory string, options deployOptions) (string, string, deployOptions, error) {
	model := newDeploymentReviewModel(plan, team, directory, options)
	program := tea.NewProgram(model, tea.WithContext(ctx),
		tea.WithInput(command.dependencies.Stdin), tea.WithOutput(command.dependencies.Stderr),
		tea.WithWindowSize(model.width, model.height))
	result, err := program.Run()
	if ctx.Err() != nil {
		return "", directory, options, ctx.Err()
	}
	if err != nil {
		return "", directory, options, fmt.Errorf("show deployment review: %w", err)
	}
	final := result.(deploymentReviewModel)
	return final.action, final.directory, final.options, nil
}

type deploymentReviewField struct {
	label string
	input textinput.Model
}

type deploymentReviewModel struct {
	plan               deploymentPlan
	team               api.Team
	directory          string
	options            deployOptions
	width, height      int
	dark, editing      bool
	selected, focused  int
	action, validation string
	fields             []deploymentReviewField
	viewport           viewport.Model
}

func newDeploymentReviewModel(plan deploymentPlan, team api.Team, directory string, options deployOptions) deploymentReviewModel {
	model := deploymentReviewModel{
		plan: plan, team: team, directory: directory, options: options,
		width: 80, height: 24, dark: true, selected: 0, // Enter defaults to deploy.
		viewport: viewport.New(),
	}
	model.refresh()
	return model
}

func (model deploymentReviewModel) Init() tea.Cmd { return tea.RequestBackgroundColor }

func (model deploymentReviewModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = max(1, message.Width), max(1, message.Height)
		model.refresh()
		return model, nil
	case tea.BackgroundColorMsg:
		model.dark = message.IsDark()
		model.styleInputs()
		model.refresh()
		return model, nil
	case tea.KeyPressMsg:
		key := message.String()
		if key == "ctrl+c" || key == "ctrl+d" {
			model.action = "cancel"
			return model, tea.Quit
		}
		if model.editing {
			switch key {
			case "esc":
				model.editing, model.validation = false, ""
				model.viewport.GotoTop()
				model.refresh()
				return model, nil
			case "tab", "down", "shift+tab", "up":
				direction := 1
				if key == "shift+tab" || key == "up" {
					direction = -1
				}
				model.fields[model.focused].input.Blur()
				model.focused = (model.focused + direction + len(model.fields)) % len(model.fields)
				cmd := model.fields[model.focused].input.Focus()
				model.refresh()
				return model, cmd
			case "enter":
				if model.saveEdits() {
					model.action = "edit"
					return model, tea.Quit
				}
				model.refresh()
				return model, nil
			}
		} else {
			switch key {
			case "esc", "q", "c", "n":
				model.action = "cancel"
				return model, tea.Quit
			case "d", "y":
				model.action = "deploy"
				return model, tea.Quit
			case "e":
				return model.startEditing()
			case "left", "shift+tab", "right", "tab":
				direction := 1
				if key == "left" || key == "shift+tab" {
					direction = -1
				}
				model.selected = (model.selected + direction + 3) % 3
				return model, nil
			case "enter":
				if model.selected == 1 {
					return model.startEditing()
				}
				model.action = "cancel"
				if model.selected == 0 {
					model.action = "deploy"
				}
				return model, tea.Quit
			}
		}
	}
	var cmd tea.Cmd
	if model.editing {
		model.fields[model.focused].input, cmd = model.fields[model.focused].input.Update(message)
		model.refresh()
	} else {
		model.viewport, cmd = model.viewport.Update(message)
	}
	return model, cmd
}

func (model deploymentReviewModel) startEditing() (tea.Model, tea.Cmd) {
	model.editing, model.focused, model.validation = true, 0, ""
	model.fields = nil
	for _, field := range []struct{ label, value, placeholder string }{
		{"Application name", model.options.name, "Use the directory name"},
		{"Branch", model.options.branch, model.plan.repository.Branch + " (current default)"},
		{"Directory", model.directory, "Path to your source"},
		{"Upstream remote", model.options.remote, "origin"},
	} {
		input := textinput.New()
		input.Prompt, input.Placeholder = "  ", field.placeholder
		input.SetValue(terminalText(field.value))
		model.fields = append(model.fields, deploymentReviewField{label: field.label, input: input})
	}
	model.styleInputs()
	cmd := model.fields[0].input.Focus()
	model.viewport.GotoTop()
	model.refresh()
	return model, cmd
}

func (model *deploymentReviewModel) styleInputs() {
	styles := textinput.DefaultStyles(model.dark)
	styles.Focused.Prompt = model.accent()
	styles.Cursor.Color = model.accent().GetForeground()
	for i := range model.fields {
		model.fields[i].input.SetStyles(styles)
	}
}

func (model *deploymentReviewModel) saveEdits() bool {
	values := make([]string, len(model.fields))
	for i, field := range model.fields {
		values[i] = strings.TrimSpace(field.input.Value())
	}
	if n := utf8.RuneCountInString(values[0]); values[0] != "" && (n < 2 || n > 80) {
		model.validation = "Application name must contain 2–80 characters."
		return false
	}
	if values[2] == "" {
		model.validation = "Choose a directory for your source."
		return false
	}
	model.options.name, model.options.branch = values[0], values[1]
	model.directory, model.options.remote = values[2], values[3]
	return true
}

func (model deploymentReviewModel) accent() lipgloss.Style {
	color := "#63E6BE"
	if !model.dark {
		color = "#087F5B"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color))
}

func (model deploymentReviewModel) muted() lipgloss.Style {
	color := "#929CAB"
	if !model.dark {
		color = "#586174"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color))
}

func (model deploymentReviewModel) framed() bool { return model.width >= 36 && model.height >= 12 }

func (model deploymentReviewModel) contentWidth() int {
	if model.framed() {
		return min(76, model.width-2) - 6 // Border and horizontal padding.
	}
	return max(1, model.width)
}

func (model deploymentReviewModel) detail(label, value string) string {
	return model.muted().Render(fmt.Sprintf("%-9s  ", label)) + terminalText(value)
}

func (model deploymentReviewModel) summary() string {
	w := model.contentWidth()
	bold := lipgloss.NewStyle().Bold(true)
	lines := []string{
		bold.Render("Welcome to Infrastry"), "",
		model.detail("App name", model.options.name),
		model.detail("Team", model.team.Ref),
		model.detail("Branch", model.plan.repository.Branch),
		model.detail("Directory", model.plan.repository.Root),
	}
	if !model.plan.managed {
		lines = append(lines, model.detail("Source", model.plan.repositoryURL))
	}
	if !model.plan.initialCommit {
		lines = append(lines, model.detail("Commit", model.plan.repository.Revision))
	}
	lines = append(lines, "", model.muted().Render("NEXT STEPS"))
	steps := []struct{ title, detail string }{}
	if model.plan.initialCommit {
		title := "Commit source files"
		if model.plan.initializeGit {
			title = "Initialize Git and commit files"
		}
		steps = append(steps, struct{ title, detail string }{title, "Stage files and create the first commit · honor .gitignore"})
	}
	steps = append(steps, struct{ title, detail string }{"Create application", "Create " + terminalText(model.options.name) + " in " + terminalText(model.team.Ref)})
	if model.plan.managed {
		detail := "Upload committed source and start deployment"
		if model.plan.repository.HostedRepositoryURL == "" {
			detail = "Add infrastry remote · upload commits · start deployment"
		}
		steps = append(steps, struct{ title, detail string }{"Upload source and deploy", detail})
	} else {
		steps = append(steps, struct{ title, detail string }{"Deploy GitHub commit", "Start deployment from the selected commit"})
	}
	for i, step := range steps {
		lines = append(lines, model.accent().Render(fmt.Sprintf("%02d  ", i+1))+bold.Render(step.title), "    "+model.muted().Render(step.detail))
	}
	return ansi.Hardwrap(strings.Join(lines, "\n"), w, true)
}

func (model *deploymentReviewModel) refresh() {
	w := model.contentWidth()
	content, focusLine := "", 0
	if model.editing {
		lines := []string{lipgloss.NewStyle().Bold(true).Render("Edit deployment settings"), model.muted().Render("Update the application name and source, then review changes."), ""}
		for i := range model.fields {
			field := &model.fields[i]
			field.input.SetWidth(max(1, w-3))
			label := model.muted().Render(field.label)
			if i == model.focused {
				label = model.accent().Bold(true).Render(field.label)
				focusLine = len(strings.Split(ansi.Hardwrap(strings.Join(lines, "\n"), w, true), "\n"))
			}
			lines = append(lines, label, field.input.View(), "")
		}
		content = ansi.Hardwrap(strings.Join(lines, "\n"), w, true)
	} else {
		content = model.summary()
	}
	reserved := 3
	if model.framed() {
		reserved = 7
	}
	model.viewport.SetWidth(w)
	model.viewport.SetHeight(max(1, min(strings.Count(content, "\n")+1, model.height-reserved)))
	model.viewport.SetContent(content)
	if model.editing {
		if focusLine < model.viewport.YOffset() {
			model.viewport.SetYOffset(focusLine)
		} else if focusLine+1 >= model.viewport.YOffset()+model.viewport.Height() {
			model.viewport.SetYOffset(focusLine + 2 - model.viewport.Height())
		}
	}
}

func (model deploymentReviewModel) actions() string {
	if model.action == "deploy" {
		return model.accent().Bold(true).Render("✓ Deployment approved")
	}
	if model.editing {
		if model.validation != "" {
			return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(model.validation)
		}
		return model.accent().Bold(true).Render("enter  Review changes") + model.muted().Render("     esc  Back")
	}
	labels := []string{"d  Deploy app ↗", "e  Edit settings", "esc  Cancel"}
	if model.contentWidth() < 58 {
		labels = []string{"d Deploy", "e Edit", "esc Cancel"}
	}
	buttons := make([]string, len(labels))
	for i, label := range labels {
		style := model.muted().Padding(0, 1)
		if i == model.selected {
			style = lipgloss.NewStyle().Bold(true).Padding(0, 1).
				Background(model.accent().GetForeground()).Foreground(lipgloss.Color("#10251D"))
			if !model.dark {
				style = style.Foreground(lipgloss.Color("#FFFFFF"))
			}
		}
		buttons[i] = style.Render(label)
	}
	return strings.Join(buttons, "  ")
}

func (model deploymentReviewModel) View() tea.View {
	if model.action == "edit" || model.action == "cancel" {
		return tea.NewView("")
	}
	w := model.contentWidth()
	brand := model.accent().Bold(true).Render("◇  INFRASTRY")
	badge := model.muted().Render("NEW APPLICATION")
	if model.editing {
		badge = model.muted().Render(fmt.Sprintf("EDIT SETTINGS · %d/%d", model.focused+1, len(model.fields)))
	}
	gap := w - ansi.StringWidth(brand) - ansi.StringWidth(badge)
	if gap >= 2 {
		brand += strings.Repeat(" ", gap) + badge
	}
	help := "← → / tab choose · enter " + []string{"deploy", "edit", "cancel"}[model.selected]
	if model.viewport.TotalLineCount() > model.viewport.Height() {
		help += " · ↑ ↓ scroll"
	}
	if model.editing {
		help = "tab / shift+tab move · ctrl+c cancel"
	}
	if model.action == "deploy" {
		help = ""
	}
	lines := []string{brand, "", model.viewport.View(), "", model.actions(), model.muted().Render(help)}
	if !model.framed() {
		lines = []string{brand, model.viewport.View(), model.actions(), model.muted().Render(help)}
	}
	for i, line := range lines {
		parts := strings.Split(line, "\n")
		for j := range parts {
			parts[j] = ansi.Truncate(parts[j], w, "…")
		}
		lines[i] = strings.Join(parts, "\n")
	}
	content := strings.Join(lines, "\n")
	if model.framed() {
		border := "#34483F"
		if !model.dark {
			border = "#B5C9BF"
		}
		content = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(border)).Padding(0, 2).Width(w + 6).Render(content)
		content = lipgloss.NewStyle().PaddingLeft(1).Render(content)
	}
	// A resized, unusually small terminal must never overflow its live region.
	rows := strings.Split(content, "\n")
	rows = rows[:min(len(rows), model.height)]
	for i := range rows {
		rows[i] = ansi.Truncate(rows[i], model.width, "…")
	}
	return tea.NewView(strings.Join(rows, "\n"))
}
