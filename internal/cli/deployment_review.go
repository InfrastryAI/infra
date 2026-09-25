package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/muesli/cancelreader"
)

var errDeploymentCancelled = errors.New("deployment cancelled before making changes")

func (command *CLI) reviewDeployment(ctx context.Context, client *api.Client, workingDirectory, directory string, team api.Team, options deployOptions) (deploymentPlan, deployOptions, error) {
	var plan deploymentPlan
	var input *bufio.Reader
	var err error
	options.requestID, err = deploymentRequestID(options.requestID)
	if err != nil {
		return plan, options, err
	}
	for {
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(workingDirectory, directory)
		}
		directory = filepath.Clean(directory)
		plan, err = command.planDeployment(ctx, client, directory, team, options)
		if err != nil {
			return plan, options, err
		}
		options.name, err = deploymentName(plan, options.name)
		if err != nil {
			return plan, options, err
		}
		// Existing applications are already associated with this deployment.
		// Only creating a new application requires review and approval.
		if plan.app.ID != "" {
			return plan, options, nil
		}
		if command.deploymentReviewUIEnabled(options) {
			var action string
			action, directory, options, err = command.showDeploymentReview(ctx, plan, team, directory, options)
			if err != nil {
				return plan, options, err
			}
			switch action {
			case "deploy":
				return plan, options, nil
			case "edit":
				continue // Resolve the edited source and app before approval.
			default:
				return plan, options, errDeploymentCancelled
			}
		}
		command.printDeploymentPlan(plan, team, options)
		if options.yes {
			return plan, options, nil
		}
		if !command.canPrompt() {
			return plan, options, usageError{"deploying a new application requires confirmation; run in an interactive terminal or pass --yes to approve the actions above"}
		}
		if input == nil {
			reader, err := cancelreader.NewReader(command.dependencies.Stdin)
			if err != nil {
				return plan, options, fmt.Errorf("read deployment confirmation: %w", err)
			}
			defer reader.Close()
			stop := context.AfterFunc(ctx, func() { reader.Cancel() })
			defer stop()
			input = bufio.NewReader(reader)
		}
		for {
			fmt.Fprint(command.dependencies.Stderr, "Deploy, edit settings, or cancel? [D/e/c]: ")
			action, err := readDeploymentInput(ctx, input)
			if err != nil {
				return plan, options, err
			}
			switch strings.ToLower(action) {
			case "", "d", "deploy", "y", "yes":
				return plan, options, nil
			case "c", "cancel", "n", "no":
				return plan, options, errDeploymentCancelled
			case "e", "edit":
				if err := command.editDeployment(ctx, input, &directory, plan, &options); err != nil {
					return plan, options, err
				}
			default:
				fmt.Fprintln(command.dependencies.Stderr, "Choose d to deploy, e to edit settings, or c to cancel.")
				continue
			}
			break // Rebuild and review the plan after editing.
		}
	}
}

func readDeploymentInput(ctx context.Context, input *bufio.Reader) (string, error) {
	line, err := input.ReadString('\n')
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, io.EOF) {
		// EOF, including an unterminated answer, never authorizes deployment.
		return "", errDeploymentCancelled
	}
	if err != nil {
		return "", fmt.Errorf("read deployment confirmation: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func (command *CLI) editDeployment(ctx context.Context, input *bufio.Reader, directory *string, plan deploymentPlan, options *deployOptions) error {
	fmt.Fprintln(command.dependencies.Stderr, "Edit deployment settings. Enter keeps the current value; - clears an optional value.")
	type editField struct {
		label string
		value *string
	}
	fields := []editField{
		{"Directory", directory},
	}
	if plan.app.ID == "" {
		fields = append(fields, editField{"New application name", &options.name})
	}
	fields = append(fields, editField{"Branch (blank uses the checked-out branch, or main for a new repository)", &options.branch})
	if plan.repository.HostedRepositoryURL == "" {
		fields = append(fields, editField{"Upstream Git remote", &options.remote})
	}
	for _, field := range fields {
		fmt.Fprintf(command.dependencies.Stderr, "%s [%s]: ", field.label, terminalText(*field.value))
		value, err := readDeploymentInput(ctx, input)
		if err != nil {
			return err
		}
		if value == "-" {
			*field.value = ""
		} else if value != "" {
			*field.value = value
		}
	}
	return nil
}
