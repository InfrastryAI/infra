package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

type NetworkDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"`
}
type NetworkService struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hostname   string `json:"hostname"`
	Address    string `json:"address"`
	Kind       string `json:"kind"`
	Ready      bool   `json:"ready"`
	Connection struct {
		Engine   string `json:"engine"`
		Username string `json:"username"`
		Database string `json:"database"`
	} `json:"connection"`
	Ports []struct {
		Protocol string `json:"protocol"`
		Port     uint16 `json:"port"`
	} `json:"ports"`
}

type DatabaseCredentials struct {
	Engine   string `json:"engine"`
	Username string `json:"username"`
	Database string `json:"database"`
	Password string `json:"password"`
}

func (c *Client) DatabaseCredentials(ctx context.Context, sessionID, serviceID string) (DatabaseCredentials, error) {
	endpoint, err := url.Parse(c.BaseURL)
	if err != nil || (endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && (endpoint.Hostname() == "localhost" || net.ParseIP(endpoint.Hostname()).IsLoopback()))) {
		return DatabaseCredentials{}, errors.New("database credential retrieval requires an HTTPS API URL")
	}
	// Keep this credential request on the authenticated API origin. Copy the
	// HTTP client so session polling and other concurrent requests are unaffected.
	httpClient := http.DefaultClient
	if c.HTTPClient != nil {
		httpClient = c.HTTPClient
	}
	privateHTTP := *httpClient
	privateHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	privateClient := *c
	privateClient.HTTPClient = &privateHTTP
	var response struct {
		Credentials DatabaseCredentials `json:"credentials"`
	}
	err = privateClient.post(ctx, "/api/v1/network-sessions/"+url.PathEscape(sessionID)+"/services/"+url.PathEscape(serviceID)+"/credentials", struct{}{}, &response)
	if err != nil {
		// Decoding failures and remote error bodies must not echo credential values.
		return DatabaseCredentials{}, errors.New("could not retrieve database credentials; check your access and reconnect")
	}
	return response.Credentials, err
}

type NetworkSession struct {
	ID         string    `json:"id"`
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name"`
	Mode       string    `json:"mode"`
	AppRef     string    `json:"app_ref"`
	Source     string    `json:"source"`
	ExpiresAt  time.Time `json:"expires_at"`
	DNS        string    `json:"dns"`
	DNSSuffix  string    `json:"dns_suffix"`
	MTU        int       `json:"mtu"`
	Gateway    struct {
		ID        string `json:"id"`
		Endpoint  string `json:"endpoint"`
		PublicKey string `json:"public_key"`
	} `json:"gateway"`
	Services []NetworkService `json:"services"`
}

func (c *Client) EnrollNetworkDevice(ctx context.Context, team, name, key, mode string) (NetworkDevice, error) {
	var response struct {
		Device NetworkDevice `json:"device"`
	}
	err := c.post(ctx, "/api/v1/network-devices", map[string]string{"team_slug": team, "name": name, "public_key": key, "mode": mode}, &response)
	return response.Device, err
}
func (c *Client) CreateNetworkSession(ctx context.Context, team, app, device string) (NetworkSession, error) {
	var response struct {
		Session NetworkSession `json:"session"`
	}
	err := c.post(ctx, "/api/v1/teams/"+url.PathEscape(team)+"/apps/"+url.PathEscape(app)+"/network-sessions", map[string]string{"device_id": device}, &response)
	return response.Session, err
}
func (c *Client) NetworkSession(ctx context.Context, id string) (NetworkSession, error) {
	var response struct {
		Session NetworkSession `json:"session"`
	}
	err := c.get(ctx, "/api/v1/network-sessions/"+url.PathEscape(id), nil, &response)
	return response.Session, err
}
func (c *Client) NetworkSessions(ctx context.Context) ([]NetworkSession, error) {
	var response struct {
		Sessions []NetworkSession `json:"sessions"`
	}
	err := c.get(ctx, "/api/v1/network-sessions", nil, &response)
	return response.Sessions, err
}
func (c *Client) DisconnectNetwork(ctx context.Context, id string) error {
	var response json.RawMessage
	return c.request(ctx, http.MethodDelete, "/api/v1/network-sessions/"+url.PathEscape(id), nil, nil, &response)
}
