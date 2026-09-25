package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type DeploymentGoal struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Stage      string            `json:"stage"`
	Message    string            `json:"message"`
	StatusPath string            `json:"status_path"`
	Action     *DeploymentAction `json:"action"`
	Result     DeploymentOutcome `json:"result"`
}

type DeploymentAction struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	Path  string `json:"path"`
}

type DeploymentOutcome struct {
	URL      string `json:"url"`
	Revision string `json:"revision"`
}

type DeploymentEvent struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"task_id"`
	Type       string    `json:"type"`
	Stage      string    `json:"stage"`
	Status     string    `json:"status,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Title      string    `json:"title"`
	Message    string    `json:"message,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	DurationMS *int64    `json:"duration_ms,omitempty"`
}

type DeploymentPage struct {
	Goal       DeploymentGoal    `json:"goal"`
	Events     []DeploymentEvent `json:"events"`
	NextCursor string            `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
}

func (client *Client) DeploymentProgress(ctx context.Context, id, cursor string) (DeploymentPage, error) {
	var page DeploymentPage
	if id == "" {
		return page, errors.New("deployment ID is required")
	}
	query := url.Values{}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	err := client.get(ctx, "/api/v1/deployments/"+url.PathEscape(id), query, &page)
	var failure *HTTPError
	if errors.As(err, &failure) && failure.StatusCode == http.StatusForbidden {
		// Existing Infrastry Git logins have apps:read/apps:deploy, rather than
		// goals:read. Both read endpoints apply the same goal ownership checks.
		err = client.get(ctx, "/api/v1/managed-deployments/"+url.PathEscape(id), query, &page)
	}
	if err != nil {
		return page, err
	}
	if page.Goal.ID != id || page.Goal.Status == "" {
		return page, errors.New("decode Infrastry response: missing or mismatched deployment identity or status")
	}
	switch page.Goal.Status {
	case "queued", "running", "needs_input", "succeeded", "failed", "cancelled":
	default:
		return page, fmt.Errorf("decode Infrastry response: unsupported deployment status %q", page.Goal.Status)
	}
	if page.HasMore && (page.NextCursor == "" || page.NextCursor == cursor) {
		return page, errors.New("deployment activity pagination did not advance")
	}
	if len(page.Events) > 0 && page.NextCursor == "" {
		return page, errors.New("deployment activity is missing a resume cursor")
	}
	for _, event := range page.Events {
		if event.ID == "" || (event.Type != "stage" && event.Type != "agent") {
			return page, errors.New("decode Infrastry response: invalid deployment activity event")
		}
	}
	return page, nil
}
