package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ErrNotRepository = errors.New("the current directory is not a Git repository")
var ErrNoCommits = errors.New("the Git repository has no commits")

type Repository struct {
	Root                string
	Branch              string
	Revision            string
	RemoteURL           string
	HostedRepositoryURL string
	Dirty               bool
}

type Control interface {
	Inspect(context.Context, string, string, string) (Repository, error)
	Initialize(context.Context, string, string, string) (Repository, error)
	PublicRemote(context.Context, Repository) (string, bool)
	EnsureRemote(context.Context, Repository, string, string) error
	Push(context.Context, Repository, string, string) error
}

type Git struct {
	HTTPClient *http.Client
}

func (Git) Inspect(ctx context.Context, directory, remoteName, requestedBranch string) (Repository, error) {
	root, exitCode, err := run(ctx, directory, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if exitCode >= 0 {
			return Repository{}, ErrNotRepository
		}
		return Repository{}, err
	}
	root, err = filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return Repository{}, fmt.Errorf("resolve Git repository root: %w", err)
	}

	branch := strings.TrimSpace(requestedBranch)
	if branch == "" {
		branch, _, err = run(ctx, root, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
		branch = strings.TrimSpace(branch)
		if err != nil || branch == "" {
			return Repository{}, errors.New("the Git checkout has a detached HEAD; pass --branch to choose the deployment branch")
		}
	}
	if err := validateRefName(ctx, root, branch); err != nil {
		return Repository{}, err
	}

	repository := Repository{
		Root: root, Branch: branch,
		RemoteURL:           remote(ctx, root, remoteName),
		HostedRepositoryURL: remote(ctx, root, "infrastry"),
	}
	revision, _, err := run(ctx, root, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		// Keep the branch, root and remotes available for deployment review
		// before an initial commit has been authorized.
		return repository, ErrNoCommits
	}
	status, _, err := run(ctx, root, nil, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return Repository{}, err
	}

	repository.Revision = strings.TrimSpace(revision)
	repository.Dirty = strings.TrimSpace(status) != ""
	return repository, nil
}

func (git Git) Initialize(ctx context.Context, directory, branch, remoteName string) (Repository, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		current, _, err := run(ctx, directory, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err == nil {
			branch = strings.TrimSpace(current)
		}
		if branch == "" {
			branch = "main"
		}
	}
	if info, err := os.Stat(directory); err != nil {
		return Repository{}, fmt.Errorf("inspect application directory: %w", err)
	} else if !info.IsDir() {
		return Repository{}, errors.New("the deployment path is not a directory")
	}
	if _, _, err := run(ctx, directory, nil, "init", "--initial-branch="+branch); err != nil {
		return Repository{}, err
	}
	files, _, err := run(ctx, directory, nil, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Repository{}, err
	}
	if strings.Trim(files, "\x00") == "" {
		return Repository{}, errors.New("the application directory has no files to deploy")
	}
	if sensitive := sensitivePaths(strings.Split(strings.TrimSuffix(files, "\x00"), "\x00")); len(sensitive) > 0 {
		return Repository{}, fmt.Errorf("refusing to commit potentially sensitive files: %s; remove them or add them to .gitignore, then retry", strings.Join(sensitive, ", "))
	}
	if _, _, err := run(ctx, directory, nil, "add", "--all"); err != nil {
		return Repository{}, err
	}
	if _, _, err := run(ctx, directory, nil,
		"-c", "user.name=Infrastry CLI",
		"-c", "user.email=cli@infrastry.ai",
		"commit", "--no-gpg-sign", "-m", "Initial deployment",
	); err != nil {
		return Repository{}, err
	}
	return git.Inspect(ctx, directory, remoteName, branch)
}

func (git Git) PublicRemote(ctx context.Context, repository Repository) (string, bool) {
	publicURL, ok := publicCloneURL(repository.RemoteURL)
	if !ok {
		return "", false
	}
	endpoint, err := url.Parse(strings.TrimRight(publicURL, "/") + "/info/refs")
	if err != nil {
		return "", false
	}
	query := endpoint.Query()
	query.Set("service", "git-upload-pack")
	endpoint.RawQuery = query.Encode()

	probeContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", false
	}
	request.Header.Set("Accept", "application/x-git-upload-pack-advertisement")
	client := git.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(next *http.Request, previous []*http.Request) error {
				if len(previous) >= 3 || next.URL.Host != previous[0].URL.Host {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/x-git-upload-pack-advertisement") {
		return "", false
	}
	advertisement, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil || advertisedRevision(advertisement, repository.Branch) != repository.Revision {
		return "", false
	}
	return publicURL, true
}

func (Git) EnsureRemote(ctx context.Context, repository Repository, name, repositoryURL string) error {
	if name == "" {
		return errors.New("hosted Git remote name cannot be empty")
	}
	if err := validateHostedURL(repositoryURL); err != nil {
		return err
	}
	existing := remote(ctx, repository.Root, name)
	if existing != "" {
		if normalizeRemote(existing) == normalizeRemote(repositoryURL) {
			return nil
		}
		return fmt.Errorf("Git remote %q already points to %s; remove or rename it before deploying", name, safeURL(existing))
	}
	_, _, err := run(ctx, repository.Root, nil, "remote", "add", name, repositoryURL)
	return err
}

func (Git) Push(ctx context.Context, repository Repository, pushURL, authorization string) error {
	if err := validateHostedURL(pushURL); err != nil {
		return err
	}
	if authorization == "" || strings.ContainsAny(authorization, "\r\n") {
		return errors.New("Infrastry returned an invalid Git push authorization")
	}
	if !validObjectID(repository.Revision) {
		return errors.New("the source commit is invalid; inspect the Git checkout and retry")
	}
	environment := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=http.extraHeader",
		"GIT_CONFIG_VALUE_1=",
		"GIT_CONFIG_KEY_2=http." + pushURL + ".extraHeader",
		"GIT_CONFIG_VALUE_2=Authorization: " + authorization,
		"GIT_CONFIG_KEY_3=http.followRedirects",
		"GIT_CONFIG_VALUE_3=false",
	}
	_, _, err := run(ctx, repository.Root, environment,
		"push", "--porcelain", "--", pushURL, repository.Revision+":refs/heads/"+repository.Branch,
	)
	if err != nil {
		return fmt.Errorf("push source to Infrastry: %w", err)
	}
	return nil
}

func validateRefName(ctx context.Context, directory, branch string) error {
	_, exitCode, _ := run(ctx, directory, nil, "check-ref-format", "--branch", branch)
	if exitCode != 0 {
		return fmt.Errorf("invalid deployment branch %q", branch)
	}
	return nil
}

func remote(ctx context.Context, directory, name string) string {
	if name == "" {
		return ""
	}
	value, _, err := run(ctx, directory, nil, "remote", "get-url", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func run(ctx context.Context, directory string, extraEnvironment []string, arguments ...string) (string, int, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	command.Env = append(os.Environ(), extraEnvironment...)
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), 0, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "", -1, errors.New("Git is required to deploy; install git and retry")
	}
	exitCode := -1
	var failure *exec.ExitError
	if errors.As(err, &failure) {
		exitCode = failure.ExitCode()
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	return string(output), exitCode, fmt.Errorf("git %s: %s", commandLabel(arguments), message)
}

func commandLabel(arguments []string) string {
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") && !strings.Contains(argument, "=") {
			return argument
		}
	}
	return "command"
}

func publicCloneURL(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if match := regexp.MustCompile(`^(?:[^@]+@)?([^:]+):(.+)$`).FindStringSubmatch(value); len(match) == 3 && !strings.Contains(value, "://") {
		value = "https://" + match[1] + "/" + strings.TrimLeft(match[2], "/")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	if parsed.Scheme == "ssh" {
		if port := parsed.Port(); port != "" && port != "22" {
			return "", false
		}
		parsed.Scheme = "https"
		if parsed.Port() == "22" {
			hostname := parsed.Hostname()
			if strings.Contains(hostname, ":") {
				parsed.Host = "[" + hostname + "]"
			} else {
				parsed.Host = hostname
			}
		}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	if parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	parsed.User = nil
	return parsed.String(), true
}

// CanonicalURL returns the credential-free HTTPS form used to compare and
// anonymously probe HTTP or SSH Git remotes.
func CanonicalURL(value string) (string, bool) {
	return publicCloneURL(value)
}

func advertisedRevision(advertisement []byte, branch string) string {
	ref := "refs/heads/" + branch
	for len(advertisement) >= 4 {
		packetLength := 0
		for _, digit := range advertisement[:4] {
			packetLength <<= 4
			switch {
			case digit >= '0' && digit <= '9':
				packetLength += int(digit - '0')
			case digit >= 'a' && digit <= 'f':
				packetLength += int(digit-'a') + 10
			case digit >= 'A' && digit <= 'F':
				packetLength += int(digit-'A') + 10
			default:
				return ""
			}
		}
		if packetLength == 0 || packetLength == 1 || packetLength == 2 {
			advertisement = advertisement[4:]
			continue
		}
		if packetLength < 4 || packetLength > len(advertisement) {
			return ""
		}
		payload := strings.TrimSuffix(string(advertisement[4:packetLength]), "\n")
		advertisement = advertisement[packetLength:]
		fields := strings.Fields(strings.SplitN(payload, "\x00", 2)[0])
		if len(fields) == 2 && fields[1] == ref && validObjectID(fields[0]) {
			return fields[0]
		}
	}
	return ""
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func validateHostedURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("Infrastry returned an invalid hosted Git URL")
	}
	return nil
}

func normalizeRemote(value string) string {
	return strings.TrimSuffix(strings.TrimSpace(value), "/")
}

func safeURL(value string) string {
	parsed, err := url.Parse(value)
	if err == nil && parsed.User != nil {
		parsed.User = nil
		return parsed.String()
	}
	return value
}

func sensitivePaths(paths []string) []string {
	var sensitive []string
	for _, path := range paths {
		base := strings.ToLower(filepath.Base(path))
		extension := strings.ToLower(filepath.Ext(base))
		environmentFile := base == ".env" || (strings.HasPrefix(base, ".env.") &&
			!strings.HasSuffix(base, ".example") &&
			!strings.HasSuffix(base, ".sample") &&
			!strings.HasSuffix(base, ".template"))
		privateKey := base == "id_rsa" || base == "id_dsa" || base == "id_ecdsa" || base == "id_ed25519" ||
			extension == ".pem" || extension == ".key" || extension == ".p12" || extension == ".pfx"
		if environmentFile || privateKey {
			sensitive = append(sensitive, path)
		}
	}
	return sensitive
}
