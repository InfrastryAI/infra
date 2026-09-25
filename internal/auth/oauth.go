package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/InfrastryAI/infra/internal/config"
)

var ErrLoginRequired = errors.New("authentication required; run `infra auth login --super` or use `--scope` for restricted access")

type LoginOptions struct {
	Scopes     []string // Friendly CLI capability names, expanded before OAuth authorization.
	Super      bool
	APIURL     string
	HTTPClient *http.Client
	OpenURL    func(string) error
	Output     io.Writer
	NoBrowser  bool
	Timeout    time.Duration
	Now        func() time.Time
}

type authorizationServerMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type registrationResponse struct {
	ClientID string `json:"client_id"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func Login(ctx context.Context, options LoginOptions) (config.Profile, error) {
	scopes, err := loginScopes(options.Scopes, options.Super)
	if err != nil {
		return config.Profile{}, err
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.Output == nil {
		options.Output = io.Discard
	}
	if options.OpenURL == nil {
		options.OpenURL = OpenBrowser
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}

	resource, server, err := discover(ctx, options.HTTPClient, options.APIURL)
	if err != nil {
		return config.Profile{}, err
	}
	if options.Super {
		scopes, err = normalizeOAuthScopes(server.ScopesSupported)
		if err != nil {
			return config.Profile{}, errors.New("the Infrastry server does not advertise a valid OAuth scope list")
		}
	}
	if missing := missingScopes(server.ScopesSupported, scopes); len(missing) > 0 {
		return config.Profile{}, fmt.Errorf("the Infrastry server does not yet advertise the required OAuth scopes: %s; advertised scopes: %s", strings.Join(missing, ", "), strings.Join(server.ScopesSupported, ", "))
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return config.Profile{}, fmt.Errorf("start local OAuth callback: %w", err)
	}
	redirectURI := "http://" + listener.Addr().String() + "/oauth/callback"

	clientID, err := registerClient(ctx, options.HTTPClient, server.RegistrationEndpoint, redirectURI)
	if err != nil {
		_ = listener.Close()
		return config.Profile{}, err
	}

	verifier, err := randomValue(64)
	if err != nil {
		_ = listener.Close()
		return config.Profile{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	state, err := randomValue(32)
	if err != nil {
		_ = listener.Close()
		return config.Profile{}, fmt.Errorf("generate OAuth state: %w", err)
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])

	result := make(chan callbackResult, 1)
	serverHTTP := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			handleCallback(writer, request, state, result)
		}),
	}
	go func() {
		_ = serverHTTP.Serve(listener)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = serverHTTP.Shutdown(shutdownCtx)
	}()

	authorizeURL, err := authorizationURL(server.AuthorizationEndpoint, clientID, redirectURI, resource.Resource, challenge, state, scopes)
	if err != nil {
		return config.Profile{}, err
	}

	if options.NoBrowser {
		fmt.Fprintf(options.Output, "Open this URL to authenticate:\n\n%s\n\nWaiting for authorization…\n", authorizeURL)
	} else if err := options.OpenURL(authorizeURL); err != nil {
		fmt.Fprintf(options.Output, "Could not open your browser automatically. Open this URL:\n\n%s\n\nWaiting for authorization…\n", authorizeURL)
	} else {
		fmt.Fprintln(options.Output, "Your browser has been opened to complete authentication.")
	}

	waitCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()

	var callback callbackResult
	select {
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return config.Profile{}, errors.New("authentication timed out; rerun your login command to try again")
		}
		return config.Profile{}, waitCtx.Err()
	case callback = <-result:
	}
	if callback.Err != nil {
		return config.Profile{}, callback.Err
	}

	tokens, err := exchangeCode(ctx, options.HTTPClient, server.TokenEndpoint, clientID, redirectURI, resource.Resource, callback.Code, verifier)
	if err != nil {
		return config.Profile{}, err
	}
	granted := tokenScopes(tokens.Scope, scopes)
	if len(missingScopes(scopes, granted)) > 0 {
		return config.Profile{}, errors.New("the Infrastry server granted permissions outside the requested scopes; no credentials were saved")
	}

	return config.Profile{
		APIURL:        options.APIURL,
		Issuer:        server.Issuer,
		Resource:      resource.Resource,
		ClientID:      clientID,
		TokenEndpoint: server.TokenEndpoint,
		AccessToken:   tokens.AccessToken,
		RefreshToken:  tokens.RefreshToken,
		TokenType:     defaultString(tokens.TokenType, "Bearer"),
		ExpiresAt:     options.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second),
		Scopes:        granted,
	}, nil
}

func discover(ctx context.Context, client *http.Client, apiURL string) (protectedResourceMetadata, authorizationServerMetadata, error) {
	var resource protectedResourceMetadata
	if err := getJSON(ctx, client, apiURL+"/.well-known/oauth-protected-resource", &resource); err != nil {
		return resource, authorizationServerMetadata{}, fmt.Errorf("discover protected resource: %w", err)
	}
	if resource.Resource == "" || len(resource.AuthorizationServers) == 0 {
		return resource, authorizationServerMetadata{}, errors.New("discover protected resource: response is missing resource or authorization_servers")
	}

	issuer := strings.TrimRight(resource.AuthorizationServers[0], "/")
	if err := validateOAuthURL(issuer); err != nil {
		return resource, authorizationServerMetadata{}, fmt.Errorf("discover authorization server: invalid issuer: %w", err)
	}
	var server authorizationServerMetadata
	if err := getJSON(ctx, client, issuer+"/.well-known/oauth-authorization-server", &server); err != nil {
		return resource, server, fmt.Errorf("discover authorization server: %w", err)
	}
	if server.Issuer == "" || server.AuthorizationEndpoint == "" || server.TokenEndpoint == "" || server.RegistrationEndpoint == "" {
		return resource, server, errors.New("discover authorization server: response is missing an endpoint")
	}
	if strings.TrimRight(server.Issuer, "/") != issuer {
		return resource, server, errors.New("discover authorization server: issuer does not match the advertised authorization server")
	}
	for _, endpoint := range []string{server.AuthorizationEndpoint, server.TokenEndpoint, server.RegistrationEndpoint} {
		if err := validateOAuthURL(endpoint); err != nil {
			return resource, server, fmt.Errorf("discover authorization server: invalid endpoint: %w", err)
		}
	}
	return resource, server, nil
}

func registerClient(ctx context.Context, client *http.Client, endpoint, redirectURI string) (string, error) {
	payload := map[string]any{
		"client_name":                "Infrastry CLI",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	var response registrationResponse
	if err := postJSON(ctx, client, endpoint, payload, &response); err != nil {
		return "", fmt.Errorf("register OAuth client: %w", err)
	}
	if response.ClientID == "" {
		return "", errors.New("register OAuth client: response is missing client_id")
	}
	return response.ClientID, nil
}

func authorizationURL(endpoint, clientID, redirectURI, resource, challenge, state string, scopes []string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse authorization endpoint: %w", err)
	}
	query := parsed.Query()
	query.Set("client_id", clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(scopes, " "))
	query.Set("resource", resource)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

type callbackResult struct {
	Code string
	Err  error
}

func handleCallback(writer http.ResponseWriter, request *http.Request, expectedState string, result chan<- callbackResult) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	if request.URL.Path != "/oauth/callback" {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	query := request.URL.Query()
	callback := callbackResult{}
	switch {
	case query.Get("state") != expectedState:
		callback.Err = errors.New("authentication callback state did not match; no credentials were saved")
	case query.Get("error") != "":
		description := defaultString(query.Get("error_description"), query.Get("error"))
		callback.Err = fmt.Errorf("authentication was not completed: %s", description)
	case query.Get("code") == "":
		callback.Err = errors.New("authentication callback did not include an authorization code")
	default:
		callback.Code = query.Get("code")
	}

	select {
	case result <- callback:
	default:
	}

	if callback.Err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, callbackPage("Authentication was not completed", "Return to your terminal and try again."))
		return
	}
	_, _ = io.WriteString(writer, callbackPage("You’re signed in", "You can close this window and return to your terminal."))
}

func callbackPage(title, message string) string {
	return "<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>" + html.EscapeString(title) + "</title></head><body style=\"margin:0;background:#101114;color:#f4f4f5;font:16px system-ui;display:grid;min-height:100vh;place-items:center\"><main style=\"max-width:34rem;padding:2rem;text-align:center\"><h1>" + html.EscapeString(title) + "</h1><p style=\"color:#a1a1aa\">" + html.EscapeString(message) + "</p></main></body></html>"
}

func exchangeCode(ctx context.Context, client *http.Client, endpoint, clientID, redirectURI, resource, code, verifier string) (tokenResponse, error) {
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"resource":      {resource},
		"code":          {code},
		"code_verifier": {verifier},
	}
	var response tokenResponse
	if err := postForm(ctx, client, endpoint, values, &response); err != nil {
		return response, fmt.Errorf("exchange authorization code: %w", err)
	}
	if response.AccessToken == "" || response.ExpiresIn <= 0 {
		return response, errors.New("exchange authorization code: response is missing access_token or expires_in")
	}
	if response.TokenType != "" && !strings.EqualFold(response.TokenType, "Bearer") {
		return response, fmt.Errorf("exchange authorization code: unsupported token type %q", response.TokenType)
	}
	return response, nil
}

type TokenManager struct {
	mu         sync.Mutex
	Profile    *config.Profile
	HTTPClient *http.Client
	Now        func() time.Time
	Persist    func(config.Profile) error
}

func (manager *TokenManager) Token(ctx context.Context) (string, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.defaults()
	if manager.Profile == nil {
		return "", ErrLoginRequired
	}
	now := manager.Now()
	if manager.Profile.AccessToken != "" && manager.Profile.ExpiresAt.After(now) &&
		(manager.Profile.RefreshToken == "" || manager.Profile.ExpiresAt.After(now.Add(30*time.Second))) {
		return manager.Profile.AccessToken, nil
	}
	return manager.refreshLocked(ctx)
}

func (manager *TokenManager) Refresh(ctx context.Context) (string, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.defaults()
	return manager.refreshLocked(ctx)
}

func (manager *TokenManager) refreshLocked(ctx context.Context) (string, error) {
	if manager.Profile == nil || manager.Profile.RefreshToken == "" || manager.Profile.ClientID == "" || manager.Profile.TokenEndpoint == "" {
		return "", ErrLoginRequired
	}
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {manager.Profile.RefreshToken},
		"client_id":     {manager.Profile.ClientID},
		"resource":      {manager.Profile.Resource},
	}
	var response tokenResponse
	if err := postForm(ctx, manager.HTTPClient, manager.Profile.TokenEndpoint, values, &response); err != nil {
		return "", fmt.Errorf("refresh authentication: %w; sign in again if the session was revoked", err)
	}
	if response.AccessToken == "" || response.ExpiresIn <= 0 {
		return "", errors.New("refresh authentication: response is missing access_token or expires_in")
	}
	if response.TokenType != "" && !strings.EqualFold(response.TokenType, "Bearer") {
		return "", fmt.Errorf("refresh authentication: unsupported token type %q", response.TokenType)
	}

	updated := *manager.Profile
	updated.AccessToken = response.AccessToken
	updated.TokenType = defaultString(response.TokenType, "Bearer")
	updated.ExpiresAt = manager.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	if response.RefreshToken != "" {
		updated.RefreshToken = response.RefreshToken
	}
	if response.Scope != "" {
		updated.Scopes = strings.Fields(response.Scope)
	}
	if manager.Persist != nil {
		if err := manager.Persist(updated); err != nil {
			return "", fmt.Errorf("save refreshed authentication: %w", err)
		}
	}
	*manager.Profile = updated
	return updated.AccessToken, nil
}

func (manager *TokenManager) defaults() {
	if manager.HTTPClient == nil {
		manager.HTTPClient = http.DefaultClient
	}
	if manager.Now == nil {
		manager.Now = time.Now
	}
}

func OpenBrowser(address string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", address)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.Command("xdg-open", address)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func randomValue(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func missingScopes(supported, required []string) []string {
	available := make(map[string]bool, len(supported))
	for _, scope := range supported {
		available[scope] = true
	}
	var missing []string
	for _, scope := range required {
		if !available[scope] {
			missing = append(missing, scope)
		}
	}
	sort.Strings(missing)
	return missing
}

func tokenScopes(value string, requested []string) []string {
	if scopes := strings.Fields(value); len(scopes) > 0 {
		return scopes
	}
	return append([]string(nil), requested...)
}

func validateOAuthURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("must be an absolute URL without user information or a fragment")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	hostname := parsed.Hostname()
	if parsed.Scheme == "http" && (hostname == "localhost" || net.ParseIP(hostname).IsLoopback()) {
		return nil
	}
	return errors.New("must use HTTPS except on loopback")
}

func getJSON(ctx context.Context, client *http.Client, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	return doJSON(client, request, target)
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, payload, target any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(encoded)))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	return doJSON(client, request, target)
}

func postForm(ctx context.Context, client *http.Client, endpoint string, values url.Values, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doJSON(client, request, target)
}

func doJSON(client *http.Client, request *http.Request, target any) error {
	// Discovery and token endpoints are validated before use. Do not let an
	// endpoint redirect an authorization code or refresh token to another URL.
	privateHTTP := *client
	privateHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := privateHTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var oauth oauthError
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&oauth)
		message := defaultString(oauth.ErrorDescription, oauth.Error)
		if message == "" {
			message = response.Status
		}
		return errors.New(message)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
