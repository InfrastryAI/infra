package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/InfrastryAI/infra/internal/api"
	"github.com/charmbracelet/x/ansi"
)

const deploymentHistoryLimit = 200

func (command *CLI) deploymentUIEnabled(options deploymentWatchOptions) bool {
	return options.follow && !options.json && command.canPrompt() &&
		outputIsTerminal(command.dependencies.Stdout) &&
		!command.noColor && command.dependencies.Getenv("NO_COLOR") == "" &&
		command.dependencies.Getenv("INFRASTRY_ACCESSIBLE") == "" &&
		command.dependencies.Getenv("CI") == "" && command.dependencies.Getenv("TERM") != "dumb"
}

type deploymentFinished struct{ err error }

func (command *CLI) watchDeploymentUI(ctx context.Context, client *api.Client, id string, options deploymentWatchOptions) error {
	watchContext, cancel := context.WithCancel(ctx)
	defer cancel()
	updates := make(chan tea.Msg, 32)
	done := make(chan struct{})
	emit := func(message tea.Msg) error {
		select {
		case updates <- message:
			return nil
		case <-watchContext.Done():
			return watchContext.Err()
		}
	}
	go func() {
		defer close(done)
		err := observeDeployment(watchContext, client, id, options, func(update deploymentUpdate) error {
			return emit(update)
		})
		_ = emit(deploymentFinished{err: err})
	}()

	model := newDeploymentModel(id, command.apiURL, options, command.dependencies.Now())
	model.next = func() tea.Msg {
		select {
		case update := <-updates:
			return update
		case <-watchContext.Done():
			return deploymentFinished{err: watchContext.Err()}
		}
	}
	program := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithInput(command.dependencies.Stdin),
		tea.WithOutput(command.dependencies.Stdout),
		tea.WithWindowSize(model.width, model.height),
	)
	result, err := program.Run()
	// Stop the read-only watcher even when input/rendering fails. Waiting here
	// keeps authentication refreshes and HTTP requests within the command lifetime.
	cancel()
	<-done
	if ctx.Err() != nil {
		return command.deploymentDetached(id)
	}
	if err != nil {
		return fmt.Errorf("show deployment progress (resume with infra deploy watch %s): %w", terminalText(id), err)
	}
	final := result.(deploymentModel)
	if final.detached {
		return command.deploymentDetached(id)
	}
	return final.err
}

type deploymentTask struct {
	id, title, stage, status string
	started, finished        time.Time
}

type deploymentModel struct {
	id, apiURL, name, source string
	goal                     api.DeploymentGoal
	tasks                    []deploymentTask
	activity                 []string
	started, now             time.Time
	width, height            int
	spinner                  spinner.Model
	viewport                 viewport.Model
	details, reconnecting    bool
	finished, detached       bool
	err                      error
	next                     tea.Cmd
}

func newDeploymentModel(id, apiURL string, options deploymentWatchOptions, now time.Time) deploymentModel {
	return deploymentModel{
		id: id, apiURL: apiURL, name: terminalText(options.name), source: terminalText(options.source),
		started: now, now: now, width: 80, height: 24,
		spinner:  spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		viewport: viewport.New(viewport.WithWidth(74), viewport.WithHeight(6)),
	}
}

func (model deploymentModel) Init() tea.Cmd {
	return tea.Batch(model.spinner.Tick, model.next)
}

func (model deploymentModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = max(1, message.Width), max(1, message.Height)
		model.refreshActivity()
	case tea.KeyPressMsg:
		switch message.String() {
		case "ctrl+c", "q":
			model.detached = true
			return model, tea.Quit
		case "d":
			model.details = !model.details
			model.refreshActivity()
			model.viewport.GotoBottom()
		default:
			if model.details {
				var cmd tea.Cmd
				model.viewport, cmd = model.viewport.Update(message)
				return model, cmd
			}
		}
	case spinner.TickMsg:
		model.now = message.Time
		var cmd tea.Cmd
		model.spinner, cmd = model.spinner.Update(message)
		return model, cmd
	case deploymentUpdate:
		if message.event != nil {
			model.recordEvent(*message.event)
		}
		if message.goal != nil {
			model.goal = *message.goal
		}
		if message.connection != "" {
			model.reconnecting = message.connection == deploymentReconnecting
		}
		return model, model.next
	case deploymentFinished:
		model.finished, model.err = true, message.err
		return model, tea.Quit
	}
	return model, nil
}

func (model *deploymentModel) recordEvent(event api.DeploymentEvent) {
	title := terminalText(event.Title)
	if title == "" {
		title = stageLabel(event.Stage)
	}
	if event.Type == "stage" {
		// Task identity distinguishes concurrent work and repeated repair attempts.
		id := event.TaskID
		if id == "" {
			id = event.Stage + ":" + title
		}
		index := -1
		for i := range model.tasks {
			if model.tasks[i].id == id {
				index = i
				break
			}
		}
		if index < 0 {
			model.tasks = append(model.tasks, deploymentTask{id: id})
			index = len(model.tasks) - 1
		}
		task := &model.tasks[index]
		task.title, task.stage, task.status = title, event.Stage, event.Status
		if event.Status == "running" {
			task.started = event.OccurredAt
			task.finished = time.Time{}
		} else {
			task.finished = event.OccurredAt
		}
		if len(model.tasks) > deploymentHistoryLimit {
			model.tasks = append([]deploymentTask(nil), model.tasks[len(model.tasks)-deploymentHistoryLimit:]...)
		}
	}
	label := stageLabel(event.Stage)
	if event.Type == "agent" {
		label = "Agent"
	}
	text := label + " · " + title
	if event.Status != "" {
		text += " · " + terminalText(event.Status)
	}
	if event.Message != "" {
		text += " — " + terminalText(event.Message)
	}
	if !event.OccurredAt.IsZero() {
		text = event.OccurredAt.Local().Format("15:04:05") + "  " + text
	}
	position, following := model.viewport.YOffset(), model.viewport.AtBottom()
	removedLines := 0
	model.activity = append(model.activity, text)
	if len(model.activity) > deploymentHistoryLimit {
		removedLines = strings.Count(ansi.Hardwrap(model.activity[0], model.viewport.Width(), true), "\n") + 1
		model.activity = append([]string(nil), model.activity[len(model.activity)-deploymentHistoryLimit:]...)
	}
	model.refreshActivity()
	if removedLines > 0 && !following {
		// Preserve the text being read when the oldest retained event expires.
		model.viewport.SetYOffset(max(0, position-removedLines))
	}
}

func (model *deploymentModel) refreshActivity() {
	follow := model.viewport.AtBottom()
	model.viewport.SetWidth(max(1, model.contentWidth()-2))
	model.viewport.SetHeight(max(1, min(8, model.height-17)))
	// Wrap activity in the details view, including long words and CJK text.
	model.viewport.SetContent(ansi.Hardwrap(strings.Join(model.activity, "\n"), model.viewport.Width(), true))
	if follow {
		model.viewport.GotoBottom()
	}
}

func (model deploymentModel) contentWidth() int {
	return max(1, min(88, model.width-4))
}

func (model deploymentModel) View() tea.View {
	width := model.contentWidth()
	muted := lipgloss.NewStyle().Faint(true)
	strong := lipgloss.NewStyle().Bold(true)
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	good := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warning := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	bad := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	line := func(text string) string { return ansi.Truncate(text, width, "…") }
	name := model.name
	if name == "" {
		name = "Deployment"
	}
	lines := []string{line(strong.Render(name) + muted.Render("  /  Infrastry"))}
	if model.source != "" {
		lines = append(lines, muted.Render(line(model.source)))
	}
	lines = append(lines, "")
	status := stageLabel(model.goal.Stage)
	if status == "" {
		status = "Connecting to deployment"
	}
	icon, style := model.spinner.View(), accent
	switch {
	case model.detached:
		icon, status, style = "○", "Watching paused", muted
	case model.finished && model.err != nil && model.goal.Status != "failed" && model.goal.Status != "cancelled" && model.goal.Status != "needs_input" && model.goal.Action == nil:
		icon, status, style = "!", "Unable to follow progress", warning
	case model.reconnecting:
		status, style = "Reconnecting · deployment continues", warning
	case model.goal.Status == "succeeded":
		icon, status, style = "✓", "Your app is live", good
	case model.goal.Status == "needs_input" || model.goal.Action != nil:
		icon, status, style = "!", "Your action is needed", warning
	case model.goal.Status == "cancelled":
		icon, status, style = "○", "Deployment cancelled", warning
	case model.goal.Status == "failed":
		icon, status, style = "×", "Deployment needs attention", bad
	}
	elapsed := ""
	if !model.finished && !model.detached {
		elapsed = muted.Render("watching " + deploymentElapsed(model.now.Sub(model.started)))
	}
	statusLine := model.columns(style.Render(icon+" "+status), elapsed)
	lines = append(lines, statusLine)
	if model.goal.Message != "" {
		message := "  " + terminalText(model.goal.Message)
		if !model.finished {
			message = line(message)
		}
		lines = append(lines, muted.Render(message))
	}

	// Keep the active display shorter than the terminal. Details own a bounded
	// viewport; completed tasks remain visible in the compact view.
	count := min(6, max(1, model.height-14))
	if model.details && !model.finished {
		count = min(count, 3)
	}
	if len(model.tasks) > 0 {
		lines = append(lines, "")
		start := max(0, len(model.tasks)-count)
		if start > 0 {
			lines = append(lines, muted.Render(line(fmt.Sprintf("  … %d earlier steps", start))))
		}
		for _, task := range model.tasks[start:] {
			marker, taskStyle := "○", muted
			switch task.status {
			case "succeeded", "completed":
				marker, taskStyle = "✓", good
			case "failed":
				marker, taskStyle = "×", bad
			case "running":
				if !model.finished && !model.detached {
					marker, taskStyle = model.spinner.View(), accent
				}
			}
			duration := ""
			if !task.started.IsZero() && (!task.finished.IsZero() || (!model.finished && !model.detached)) {
				end := task.finished
				if end.IsZero() {
					end = model.now
				}
				duration = deploymentElapsed(end.Sub(task.started))
			}
			lines = append(lines, model.columns("  "+taskStyle.Render(marker)+" "+task.title, muted.Render(duration)))
		}
	}
	if len(model.activity) > 0 && !model.finished && !model.detached && model.height >= 18 {
		lines = append(lines, "")
		if model.details {
			lines = append(lines, muted.Render(line(fmt.Sprintf("Activity · %d recent events", len(model.activity)))))
			lines = append(lines, model.viewport.View())
		} else {
			lines = append(lines, muted.Render(line(model.activity[len(model.activity)-1])))
		}
	}
	if model.finished && model.err != nil && len(model.activity) > 0 {
		lines = append(lines, "", muted.Render("Recent activity"))
		for _, activity := range model.activity[max(0, len(model.activity)-3):] {
			lines = append(lines, muted.Render(activity))
		}
	}
	if model.goal.Result.URL != "" || model.goal.Action != nil {
		lines = append(lines, "")
		if model.goal.Result.URL != "" {
			lines = append(lines, good.Render(terminalText(model.goal.Result.URL)))
		}
		if action := model.goal.Action; action != nil {
			lines = append(lines, strong.Render(terminalText(action.Label)), model.link(action.Path))
		}
	}
	lines = append(lines, "")
	if !model.finished && !model.detached {
		help := "d details · ctrl+c detach"
		if model.details {
			help = "d collapse · ↑↓ scroll · ctrl+c detach"
		}
		lines = append(lines, muted.Render(line(help)))
	}
	// A very short terminal gets only the current state and keyboard hint.
	if model.height < 12 && !model.finished && !model.detached {
		lines = []string{statusLine}
		if model.height >= 4 {
			lines = append(lines, muted.Render(line("d details · ctrl+c detach")))
		}
	}
	// Inline rendering leaves a useful final receipt in terminal scrollback.
	content := ansi.Hardwrap(strings.Join(lines, "\n"), width, true)
	content = lipgloss.NewStyle().PaddingLeft(min(2, max(0, model.width-1))).Render(content)
	if model.height >= 12 || model.finished || model.detached {
		content = "\n" + content + "\n"
	}
	return tea.NewView(content)
}

func (model deploymentModel) columns(left, right string) string {
	width := model.contentWidth()
	rightWidth := ansi.StringWidth(right)
	if width < rightWidth+12 || right == "" {
		return ansi.Truncate(left, width, "…")
	}
	left = ansi.Truncate(left, width-rightWidth-2, "…")
	return left + strings.Repeat(" ", max(2, width-ansi.StringWidth(left)-rightWidth)) + right
}

func (model deploymentModel) link(path string) string {
	return strings.TrimRight(model.apiURL, "/") + "/" + strings.TrimLeft(terminalText(path), "/")
}

func deploymentElapsed(duration time.Duration) string {
	seconds := int(max(0, duration.Seconds()))
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
}
