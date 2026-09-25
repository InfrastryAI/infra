package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/spf13/cobra"
)

type deploymentWatchOptions struct {
	json, follow bool
	poll         time.Duration
	name, source string
}

func (command *CLI) deployWatchCommand() *cobra.Command {
	options := deploymentWatchOptions{}
	watch := &cobra.Command{
		Use:   "watch <deployment-id>",
		Short: "Reconnect to a deployment's stages and agent activity",
		Long:  "Replay saved activity and follow the deployment. Ctrl-C stops watching; it never cancels the deployment.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if options.poll < 100*time.Millisecond {
				return usageError{"--poll must be at least 100ms"}
			}
			client, err := command.apiClient()
			if err != nil {
				return err
			}
			command.printDeploymentResume(args[0])
			return command.watchDeployment(cmd.Context(), client, args[0], options)
		},
	}
	watch.Flags().BoolVar(&options.json, "json", false, "emit newline-delimited JSON events")
	watch.Flags().BoolVar(&options.follow, "follow", true, "follow until completion (false replays current activity and exits)")
	watch.Flags().DurationVar(&options.poll, "poll", time.Second, "interval between progress checks")
	return watch
}

func deploymentRequestID(requested string) (string, error) {
	if requested != "" {
		if len(requested) < 8 || len(requested) > 255 || terminalText(requested) != requested {
			return "", usageError{"--request-id must contain 8 to 255 printable characters"}
		}
		return requested, nil
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("create deployment submission ID: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (command *CLI) printDeploymentResume(id string) {
	fmt.Fprintf(command.dependencies.Stderr, "Deployment runs on Infrastry; closing this terminal will not stop it.\nResume: infra deploy watch %s\n", terminalText(id))
}

func (command *CLI) watchDeployment(ctx context.Context, client *api.Client, id string, options deploymentWatchOptions) error {
	if command.deploymentUIEnabled(options) {
		return command.watchDeploymentUI(ctx, client, id, options)
	}
	err := observeDeployment(ctx, client, id, options, func(update deploymentUpdate) error {
		switch {
		case update.event != nil:
			return command.printDeploymentEvent(*update.event, options.json)
		case update.goal != nil:
			return command.printDeploymentGoal(*update.goal, options.json)
		case update.connection != "":
			_, err := fmt.Fprintln(command.dependencies.Stderr, update.connection)
			return err
		}
		return nil
	})
	if ctx.Err() != nil {
		return command.deploymentDetached(id)
	}
	return err
}

// Both renderers consume the same ordered, deduplicated activity. The observer
// owns polling and retry semantics; it never writes to a terminal itself.
type deploymentUpdate struct {
	event      *api.DeploymentEvent
	goal       *api.DeploymentGoal
	connection string
}

const deploymentReconnecting = "Progress connection interrupted. Deployment continues on Infrastry; reconnecting…"
const deploymentReconnected = "Progress connection restored."

func observeDeployment(ctx context.Context, client *api.Client, id string, options deploymentWatchOptions, emit func(deploymentUpdate) error) error {
	cursor, lastGoal := "", ""
	seen := make(map[string]struct{})
	backoff := options.poll
	failures := 0
	for {
		page, err := client.DeploymentProgress(ctx, id, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !retryable(err) {
				return err
			}
			failures++
			if failures == 3 {
				if err := emit(deploymentUpdate{connection: deploymentReconnecting}); err != nil {
					return err
				}
			}
			if err := wait(ctx, backoff); err != nil {
				return err
			}
			backoff = minDuration(backoff*2, 30*time.Second)
			continue
		}
		if failures >= 3 {
			if err := emit(deploymentUpdate{connection: deploymentReconnected}); err != nil {
				return err
			}
		}
		failures = 0
		backoff = options.poll
		for _, event := range page.Events {
			if _, exists := seen[event.ID]; exists {
				continue
			}
			if err := emit(deploymentUpdate{event: &event}); err != nil {
				return err
			}
			seen[event.ID] = struct{}{}
		}
		// Keep only the current page for overlap/retry de-duplication. Older events
		// precede the acknowledged cursor and cannot appear in a subsequent page.
		if len(seen) > 2000 {
			seen = make(map[string]struct{}, len(page.Events))
			for _, event := range page.Events {
				seen[event.ID] = struct{}{}
			}
		}
		cursor = page.NextCursor
		if page.HasMore {
			continue
		}
		goal := page.Goal
		fingerprint := fmt.Sprintf("%s\x00%s\x00%s\x00%v\x00%s", goal.Status, goal.Stage, goal.Message, goal.Action, goal.Result.URL)
		if fingerprint != lastGoal {
			if err := emit(deploymentUpdate{goal: &goal}); err != nil {
				return err
			}
			lastGoal = fingerprint
		}
		switch goal.Status {
		case "succeeded":
			return nil
		case "failed":
			return errors.New("deployment failed; review the activity above and retry after resolving the problem")
		case "cancelled":
			return errors.New("deployment was cancelled")
		case "needs_input":
			return errors.New("deployment needs your input; continue on Infrastry")
		}
		if goal.Action != nil {
			return errors.New("deployment is waiting for your action; it will continue on Infrastry once resolved")
		}
		if !options.follow {
			return nil
		}
		if err := wait(ctx, options.poll); err != nil {
			return err
		}
	}
}

func (command *CLI) deploymentDetached(id string) error {
	fmt.Fprintf(command.dependencies.Stderr, "Stopped watching. Deployment continues on Infrastry.\nResume: infra deploy watch %s\n", terminalText(id))
	return nil
}

func (command *CLI) printDeploymentEvent(event api.DeploymentEvent, jsonOutput bool) error {
	if jsonOutput {
		return writeDeploymentJSON(command.dependencies.Stdout, event)
	}
	label := stageLabel(event.Stage)
	if event.Type == "agent" {
		label = "Agent · " + label
	}
	text := terminalText(event.Title)
	if event.Status != "" {
		text += " — " + terminalText(event.Status)
	}
	if event.Message != "" {
		text += ": " + terminalText(event.Message)
	}
	_, err := fmt.Fprintf(command.dependencies.Stdout, "%s  [%s] %s\n", event.OccurredAt.Local().Format("15:04:05"), label, text)
	return err
}

func (command *CLI) printDeploymentGoal(goal api.DeploymentGoal, jsonOutput bool) error {
	if jsonOutput {
		return writeDeploymentJSON(command.dependencies.Stdout, map[string]any{"type": "status", "goal": goal})
	}
	if _, err := fmt.Fprintf(command.dependencies.Stdout, "\n%s  %s\n", command.statusStyle(stageLabel(goal.Stage)), terminalText(goal.Message)); err != nil {
		return err
	}
	if goal.Result.URL != "" {
		fmt.Fprintf(command.dependencies.Stdout, "URL: %s\n", terminalText(goal.Result.URL))
	}
	if goal.Action != nil {
		fmt.Fprintf(command.dependencies.Stdout, "%s: %s\n", terminalText(goal.Action.Label), command.deploymentLink(goal.Action.Path))
	}
	return nil
}

func (command *CLI) deploymentLink(path string) string {
	return strings.TrimRight(command.apiURL, "/") + "/" + strings.TrimLeft(terminalText(path), "/")
}

func stageLabel(stage string) string {
	labels := map[string]string{
		"queued": "Queued", "resolving": "Preparing", "preparing": "Preparing", "checkout": "Source",
		"analysis": "Analysis", "analyzing": "Analysis", "planning": "Planning", "app_spec": "Planning",
		"dockerfile": "Build plan", "building": "Build", "build": "Build", "prepare": "Preparing",
		"apply": "Provisioning", "provider": "Provisioning", "provisioning": "Provisioning",
		"verify": "Health checks", "verifying": "Health checks", "activate": "Publishing",
		"debugging": "Repair", "repairing": "Repair", "complete": "Complete", "failed": "Failed",
		"cancelled": "Cancelled", "awaiting_billing": "Payment method needed",
	}
	if label, ok := labels[stage]; ok {
		return label
	}
	return terminalText(strings.ReplaceAll(stage, "_", " "))
}

// Remote titles, paths and names must not inject terminal control sequences.
func terminalText(value string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf) {
			return -1
		}
		return char
	}, value)
}

func writeDeploymentJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
