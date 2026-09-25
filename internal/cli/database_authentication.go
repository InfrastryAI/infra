package cli

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// The password is never an argument, environment value, or saved CLI setting.
// The caller removes this private directory after the client exits, including
// cancellation and failure paths. A forced process kill can prevent cleanup.
func databaseAuthentication(options proxyOptions, address, password string) (arguments, environment []string, cleanup func(), err error) {
	_, arguments, err = databaseClient(options, address)
	if err != nil {
		return nil, nil, nil, err
	}
	if password == "" || strings.ContainsAny(password, "\x00\r\n") {
		return nil, nil, nil, errors.New("the database returned an unsupported password; use --password-prompt")
	}
	host, port, _ := net.SplitHostPort(address)
	var content string
	switch options.engine {
	case "postgres":
		escape := strings.NewReplacer(`\`, `\\`, ":", `\:`)
		fields := []string{host, port, options.databaseName, options.user, password}
		for i := range fields {
			fields[i] = escape.Replace(fields[i])
		}
		content = strings.Join(fields, ":") + "\n"
	case "mysql":
		escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\t", `\t`, "\b", `\b`)
		content = "[client]\npassword=\"" + escape.Replace(password) + "\"\n"
	}
	directory, err := os.MkdirTemp("", "infra-db-credentials-")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup = func() { os.RemoveAll(directory) }
	path := filepath.Join(directory, "credentials")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	// Inherited libpq/MySQL settings must not override the selected tunnel or password.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PG") && !strings.HasPrefix(entry, "MYSQL_") {
			environment = append(environment, entry)
		}
	}
	if options.engine == "postgres" {
		arguments[1] = "-w"
		environment = append(environment, "PGPASSFILE="+path)
	} else {
		authenticated := []string{"--defaults-file=" + path, "--no-login-paths"}
		for _, argument := range arguments {
			if argument != "--password" {
				authenticated = append(authenticated, argument)
			}
		}
		arguments = authenticated
	}
	return arguments, environment, cleanup, nil
}
