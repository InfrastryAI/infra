package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/InfrastryAI/infra/internal/api"
	"github.com/charmbracelet/x/ansi"
)

func TestDeploymentUITracksTaskIdentityAndActualDurations(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	model := newDeploymentModel("goal", "https://infrastry.test", deploymentWatchOptions{name: "Customer API"}, now)
	model.now = now.Add(20 * time.Second)
	for _, event := range []api.DeploymentEvent{
		{TaskID: "attempt-1", Type: "stage", Stage: "build", Title: "Build image", Status: "running", OccurredAt: now},
		{TaskID: "attempt-1", Type: "stage", Stage: "build", Title: "Build image", Status: "failed", OccurredAt: now.Add(4 * time.Second)},
		{TaskID: "attempt-2", Type: "stage", Stage: "build", Title: "Build repaired image", Status: "running", OccurredAt: now.Add(8 * time.Second)},
		{TaskID: "database", Type: "stage", Stage: "apply", Title: "Prepare database", Status: "running", OccurredAt: now.Add(9 * time.Second)},
	} {
		model.recordEvent(event)
	}
	output := ansi.Strip(model.View().Content)
	if len(model.tasks) != 3 || model.tasks[0].status != "failed" || model.tasks[1].status != "running" {
		t.Fatalf("lost a retry or inferred a task outcome: %+v", model.tasks)
	}
	for _, want := range []string{"× Build image", "4s", "Build repaired image", "12s", "Prepare database", "11s"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in:\n%s", want, output)
		}
	}
	if model.View().AltScreen {
		t.Fatal("deployment must preserve terminal scrollback")
	}
}

func TestDeploymentUIDetailsFollowOnlyAtBottomAndBoundHistory(t *testing.T) {
	model := newDeploymentModel("goal", "https://infrastry.test", deploymentWatchOptions{}, time.Now())
	for i := range 240 {
		model.recordEvent(api.DeploymentEvent{Type: "agent", Title: fmt.Sprintf("Activity %03d", i)})
	}
	updated, _ := model.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	model = updated.(deploymentModel)
	if !model.details || !model.viewport.AtBottom() || len(model.activity) != deploymentHistoryLimit {
		t.Fatal("details did not open at the end of bounded history")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	model = updated.(deploymentModel)
	visible := model.viewport.View()
	model.recordEvent(api.DeploymentEvent{Type: "agent", Title: "Newest activity"})
	if model.viewport.View() != visible || model.viewport.AtBottom() {
		t.Fatal("new activity interrupted reading older output")
	}
	model.viewport.GotoBottom()
	model.recordEvent(api.DeploymentEvent{Type: "agent", Title: "Following again"})
	if !strings.Contains(ansi.Strip(model.viewport.View()), "Following again") {
		t.Fatal("activity did not follow at bottom")
	}
}

func TestDeploymentUILiveViewFitsTerminal(t *testing.T) {
	for _, width := range []int{1, 10, 24, 40, 80, 120} {
		for _, height := range []int{1, 4, 8, 12, 16, 20, 24, 40} {
			for _, details := range []bool{false, true} {
				t.Run(fmt.Sprintf("%dx%d/details=%t", width, height, details), func(t *testing.T) {
					model := newDeploymentModel("goal", "https://infrastry.test", deploymentWatchOptions{name: strings.Repeat("界", 60), source: "acme/api · main · abc1234"}, time.Now())
					model.width, model.height, model.details = width, height, details
					model.goal = api.DeploymentGoal{Stage: "building", Status: "running", Message: "Installing application dependencies"}
					for i := range 20 {
						model.recordEvent(api.DeploymentEvent{TaskID: fmt.Sprint(i), Type: "stage", Stage: "build", Title: strings.Repeat("Long title ", 30), Status: "running"})
					}
					lines := strings.Split(model.View().Content, "\n")
					if len(lines) > height {
						t.Errorf("view has %d lines for height %d:\n%s", len(lines), height, ansi.Strip(model.View().Content))
					}
					for _, line := range lines {
						if ansi.StringWidth(line) > width {
							t.Errorf("line exceeds width %d: %q", width, ansi.Strip(line))
						}
					}
				})
			}
		}
	}
}

func TestDeploymentUIFinalOutcomesKeepUsefulReceipt(t *testing.T) {
	for _, test := range []struct{ status, heading string }{
		{"succeeded", "Your app is live"}, {"failed", "Deployment needs attention"},
		{"needs_input", "Your action is needed"}, {"cancelled", "Deployment cancelled"},
	} {
		t.Run(test.status, func(t *testing.T) {
			model := newDeploymentModel("goal", "https://infrastry.test", deploymentWatchOptions{}, time.Now())
			model.goal = api.DeploymentGoal{Status: test.status, Stage: "complete", Message: "Current outcome", StatusPath: "/activity/goal"}
			var err error
			if test.status == "succeeded" {
				model.goal.Result.URL = "https://app.example"
			} else {
				err = errors.New("deployment needs attention")
			}
			if test.status == "needs_input" {
				model.goal.Action = &api.DeploymentAction{Label: "Add payment method", Path: "/billing"}
			}
			model.recordEvent(api.DeploymentEvent{Type: "agent", Title: "Review application settings"})
			updated, _ := model.Update(deploymentFinished{err: err})
			model = updated.(deploymentModel)
			output := ansi.Strip(model.View().Content)
			for _, want := range []string{test.heading, "Current outcome"} {
				if !strings.Contains(output, want) {
					t.Errorf("missing %q in %s", want, output)
				}
			}
			if strings.Contains(output, "ctrl+c") || strings.Contains(output, "watching ") {
				t.Errorf("finished view still looks active: %s", output)
			}
			if err != nil && !strings.Contains(output, "Review application settings") {
				t.Errorf("failure discarded activity: %s", output)
			}
			if test.status == "needs_input" && !strings.Contains(output, "https://infrastry.test/billing") {
				t.Errorf("action link missing: %s", output)
			}
			if test.status == "succeeded" && !strings.Contains(output, "https://app.example") {
				t.Errorf("application URL missing: %s", output)
			}
		})
	}
}

func TestDeploymentUIReconnectionAndRemoteText(t *testing.T) {
	model := newDeploymentModel("goal", "https://infrastry.test", deploymentWatchOptions{}, time.Now())
	model.recordEvent(api.DeploymentEvent{Type: "agent", Title: "Read\x1b]52;c;secret\a source\r\n", Message: "settings\u202e"})
	for _, value := range model.activity {
		if strings.ContainsAny(value, "\x1b\a\r\n\u202e") {
			t.Fatalf("remote text retained terminal controls: %q", value)
		}
	}
	updated, _ := model.Update(deploymentUpdate{connection: deploymentReconnecting})
	model = updated.(deploymentModel)
	if !strings.Contains(ansi.Strip(model.View().Content), "Reconnecting") {
		t.Fatal("lost connection was not visible")
	}
	updated, _ = model.Update(deploymentUpdate{connection: deploymentReconnected})
	model = updated.(deploymentModel)
	if strings.Contains(ansi.Strip(model.View().Content), "Reconnecting") {
		t.Fatal("reconnected view still appears offline")
	}
}

type deploymentUITokens struct{}

func (deploymentUITokens) Token(context.Context) (string, error)   { return "test-token", nil }
func (deploymentUITokens) Refresh(context.Context) (string, error) { return "test-token", nil }

func TestDeploymentObserverReportsReconnectWithoutLosingCursor(t *testing.T) {
	calls := 0
	client := &api.Client{BaseURL: "https://infrastry.test", Tokens: deploymentUITokens{}, HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 && request.URL.Query().Get("cursor") != "c1" {
			t.Errorf("lost cursor during retry: %s", request.URL)
		}
		if calls >= 2 && calls <= 4 {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
		}
		goal := api.DeploymentGoal{ID: "goal", Status: "running", Stage: "building"}
		if calls >= 5 {
			goal.Status = "succeeded"
		}
		page := api.DeploymentPage{Goal: goal, Events: []api.DeploymentEvent{{ID: "event", Type: "agent", Title: "Reading source"}}, NextCursor: "c1"}
		body, _ := json.Marshal(page)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	var connections []string
	events := 0
	err := observeDeployment(t.Context(), client, "goal", deploymentWatchOptions{follow: true, poll: time.Millisecond}, func(update deploymentUpdate) error {
		if update.connection != "" {
			connections = append(connections, update.connection)
		}
		if update.event != nil {
			events++
		}
		return nil
	})
	if err != nil || calls != 5 || events != 1 {
		t.Fatalf("watch failed or replayed activity: calls=%d events=%d error=%v", calls, events, err)
	}
	if len(connections) != 2 || connections[0] != deploymentReconnecting || connections[1] != deploymentReconnected {
		t.Fatalf("missing connection state transitions: %v", connections)
	}
}

func TestDeploymentUIProgramCompletesAndDetachesWithoutMutations(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "needs_input", "cancelled", "detach", "cancel-context"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			input, keyboard := io.Pipe()
			defer input.Close()
			defer keyboard.Close()
			var stdout, stderr bytes.Buffer
			requestStarted, requestStopped := make(chan struct{}), make(chan struct{})
			client := &api.Client{BaseURL: "https://infrastry.test", Tokens: deploymentUITokens{}, HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				defer close(requestStopped)
				if request.Method != "GET" {
					t.Errorf("watch mutated deployment: %s", request.Method)
				}
				close(requestStarted)
				if outcome == "detach" || outcome == "cancel-context" {
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				body, _ := json.Marshal(api.DeploymentPage{Goal: api.DeploymentGoal{ID: "goal", Status: outcome, Stage: "complete", Result: api.DeploymentOutcome{URL: "https://app.example"}}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})}}
			if outcome == "detach" || outcome == "cancel-context" {
				go func() {
					<-requestStarted
					if outcome == "detach" {
						_, _ = keyboard.Write([]byte("q"))
					} else {
						cancel()
					}
				}()
			}
			command := New(Dependencies{Stdin: input, Stdout: &stdout, Stderr: &stderr})
			command.apiURL = client.BaseURL
			err := command.watchDeploymentUI(ctx, client, "goal", deploymentWatchOptions{follow: true, poll: time.Millisecond})
			wantError := outcome == "failed" || outcome == "needs_input" || outcome == "cancelled"
			if (err != nil) != wantError {
				t.Fatalf("outcome %s: %v\n%s", outcome, err, ansi.Strip(stdout.String()))
			}
			select {
			case <-requestStopped:
			default:
				t.Fatal("watch returned before its request stopped")
			}
			if outcome == "detach" || outcome == "cancel-context" {
				if !strings.Contains(stderr.String(), "Deployment continues") {
					t.Fatalf("detach did not explain how to resume: %s", stderr.String())
				}
			} else if outcome == "succeeded" && !strings.Contains(ansi.Strip(stdout.String()), "Your app is live") {
				t.Fatalf("final frame was not rendered: %s", stdout.String())
			}
		})
	}
}
