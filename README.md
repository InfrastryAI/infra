# Infrastry CLI

`infra` brings the everyday Infrastry control-plane workflow to the terminal:
browser-based authentication, application discovery, direct database connections, 
and live customer logs.

## Build

```sh
go build -o infra .
./infra --help
```

## Authenticate

```sh
infra auth login --super
infra auth status
infra auth logout
```

`--super` is the recommended setup for your own computer. It requests every normal 
OAuth scope advertised by the server, enabling all CLI tools within your existing 
team permissions. It does not grant a higher team role.

Login uses OAuth authorization code with PKCE `S256`, a random-state check, and
a temporary loopback callback. The CLI never asks for an Infrastry password.
Use `--no-browser` on a machine where the browser must be opened manually.

Credentials are separated by installation. OAuth tokens are stored in the
operating system credential manager (macOS Keychain, Linux Secret Service, or
Windows Credential Manager). If a keyring is unavailable—for example on a
headless Linux server—the CLI falls back to its private configuration file and
prints a warning. On Linux that directory is `~/.config/infrastry`, with mode
`0700`, and `config.json` has mode `0600`. Existing file-based tokens migrate
to the keyring automatically. Logins request refresh access so the CLI can refresh
expiring access tokens without another browser login.

For persistent local development, select the API URL once and then
authenticate normally:

```sh
infra config set api http://localhost:4000
infra auth login --super
```

Use `infra config get api` to show the selected URL. Switch back with
`infra config set api https://infrastry.ai`.

For a one-command staging or local override, put the global flag before the
command:

```sh
infra --api-url http://localhost:4000 auth login --super
```

`INFRASTRY_API_URL` and `INFRASTRY_CONFIG` provide equivalent environment
overrides. `NO_COLOR` disables terminal styling, `INFRASTRY_ACCESSIBLE=1`
uses screen-reader-friendly prompts, and `--no-input` disables all interactive
prompts for automation.

## Restricted installations

Use `--scope` when installing the CLI in an environment that should only have
selected capabilities. Supply friendly names as a comma-separated list:

```sh
infra auth login --scope logs
infra auth login --scope network
infra auth login --scope database,logs
```

| Option | Enables |
| --- | --- |
| `apps` | List teams, applications, and private services. |
| `logs` | Read application logs. |
| `deploy` | Start, inspect, continue, and cancel deployment goals, and read logs. |
| `network` | Connect to private services, use local proxies, and manage connection devices. |
| `database` | Everything in `network`, plus automatic database login using the deployed credentials. |

Every option includes app discovery and a renewable login. The CLI requests the
API permissions needed for each capability automatically. `database` includes
network access, so you do not need to select both. `network` supports `db proxy`
and `db connect --password-prompt` without retrieving database credentials.
Team permissions still apply; database credential access requires an owner/admin
role.

Choose either `--super` or `--scope`; they cannot be combined. Bare
`infra auth login` asks you to choose. `--scope` accepts only the friendly names
above. Signing in again replaces the selected installation's saved login rather
than adding to its previous permissions. `infra auth status` shows the actual
granted API permissions.

## Shell completion

Generate and load completions for your shell:

```sh
# Bash
source <(infra completion bash)

# Zsh (run `compinit` first if it is not already enabled)
source <(infra completion zsh)

# Fish
infra completion fish | source

# PowerShell
infra completion powershell | Out-String | Invoke-Expression
```

Add the matching command to your shell startup file to enable completions in
future sessions. Completions are generated from the Cobra command tree, so
commands, flags, and help stay synchronized. Authenticated completions also
suggest available teams and applications, with a short timeout so a slow API
does not block the shell.

## Select a team

Every operational command after authentication runs in one team context. On
the first interactive command, the CLI prompts when multiple teams are
available and stores the selection. In non-interactive use it deterministically
selects the first team returned by Infrastry. View or change it at any time:

```sh
infra config get team
infra teams list
infra config set team acme
infra config set team       # interactive picker
```

Team slugs are immutable and globally unique. Team names are also matched
case-insensitively; use the canonical team ref (its slug) when names are
ambiguous.

## List applications

```sh
infra apps list
infra apps list --status healthy
infra apps list --json
```

Human output is a status table with running containers nested beneath each app
using tree branches. Container names, runtimes, and statuses align with the app's
slug, name, and status columns. Long container names are shortened with
an ellipsis to keep the table compact. JSON mode preserves full names and IDs
and includes container images and runtime labels for scripts and automation.
Runtime labels match the app overview (for example, `Node.js` or `Elixir · OTP 27`);
containers without a matching runtime show `—`.
Container lookup failures appear as unavailable for that app;
an empty container list appears as `No running containers`.

## Deploy the current directory

Run `deploy` from your application directory:

```sh
infra deploy
infra deploy --name "Customer API"
infra deploy ./path/to/app
infra deploy --detach
infra deploy watch <deployment-id>
infra deploy watch <deployment-id> --follow=false
infra deploy --json
infra deploy --yes --no-input --json
```

When a deployment would create a new application, the CLI shows the directory,
team, application, branch, commit (when available), and
planned actions before changing Git state or creating the app. Choose **deploy**,
**edit settings**, or **cancel**. Edited settings are checked again; if they still
create a new application, an updated review appears. Enter deploys by default;
Ctrl-C or end-of-input cancels. To edit source files or `.gitignore`, cancel and
rerun when ready.

In an interactive terminal, the review appears as a card with your destination,
source, and planned actions. **App name** shows the name that will be submitted
when creating the application; it defaults from the directory name and can be
changed with `--name` or Edit settings. Press `d` to deploy, `e` to edit settings in place,
or `Esc` to cancel. You can also select an action with Left/Right or Tab and
confirm with Enter (Deploy app is selected initially). In the editor, Tab moves
between fields, Enter reviews your changes, and Esc discards pending edits.
Long reviews scroll with Up/Down. Accessible terminals, `--no-color`, `NO_COLOR`,
`TERM=dumb`, CI, and JSON output use the plain-text review.

Deployments to an existing application proceed without confirmation, whether
the app is linked by an `infrastry` remote or matched by repository URL and branch.

Non-interactive deployments that create a new application require `--yes` (or
`-y`). `--no-input`, `--json`, and `--detach` do not imply approval for a new app.
Review and prompt output goes to stderr, keeping JSON on stdout machine-readable.

By default, `deploy` follows the deployment through source analysis, planning,
building, provisioning, health checks, and publication. Stage transitions and
agent actions appear as they happen, checked once per second. The activity view
includes the same redacted agent descriptions as the web application.

In an interactive terminal, deployment progress updates in place with animated
steps, task durations, and the latest activity. Press `d` to expand the recent
activity, then use the arrow keys or Page Up/Page Down to scroll. New activity
follows automatically while you are at the bottom. Press `d` again to collapse
the details. The final result and application URL stay in your terminal.

Piped output, CI, `--no-input`, `--no-color`, `NO_COLOR`,
`INFRASTRY_ACCESSIBLE=1`, `TERM=dumb`, and `--follow=false` use a plain activity
transcript. `--json` continues to emit newline-delimited JSON without animation.

Once accepted, the deployment runs on Infrastry. Closing the terminal, losing
the connection, or pressing Ctrl-C only stops watching. `--detach` returns as
soon as the server has durably accepted the request. The CLI prints a deployment
ID and an `infra deploy watch <deployment-id>` command; watching replays saved
activity before following new events and never starts another deployment.
Use `--follow=false` for a one-time view. Watchers refresh authentication and
retry temporary connection failures with bounded backoff.

`--json` emits one JSON object per line: an `accepted` record, `stage` and `agent`
activity records with stable IDs, and `status` records. Diagnostics and reconnect
instructions go to stderr. `--detach --json` emits only the acceptance record.
Successful deployments exit zero; failed/cancelled deployments and requests
waiting for user action exit nonzero. Billing setup includes an action link;
the server continues the same request automatically once billing is ready.

Each submission gets an idempotency key, printed before the CLI changes local
Git state or sends a request. For local source, the updated Infrastry server
uses the same key to create or recover the application and to submit the
deployment. If the connection drops before acceptance, retry the same source
and settings with `infra deploy --request-id <submission-id>` to recover the
request without creating another application or deployment. Submission IDs and
deployment IDs serve different purposes.

For a folder without Git history, the CLI initializes Git and creates an initial
commit, honoring `.gitignore` and refusing likely secret files such as `.env`.
For local source without an upstream remote, it creates an application, saves
an `infrastry` Git remote, and uploads the source using your existing login.
Later deploys reuse that remote and application, including retries after a
failed upload.

An existing `infrastry` Git remote takes precedence over other remotes. It must
belong to the selected Infrastry installation and team, and the destination
branch must match the application. Otherwise, for public GitHub source, the CLI
verifies that the current commit exists on the selected remote branch and that
the repository can be cloned anonymously.
Use `infra auth login --scope deploy` or `--super` if your existing login does
not grant source upload permission. Private upstream repositories without an
Infrastry Git remote are not supported.

Existing repositories with commits must have a clean working tree so the
deployed revision is unambiguous. Commit or ignore subsequent changes before
deploying again.

For GitHub source, an existing application with the same normalized repository
URL and branch is deployed again; otherwise a new application is created.
Infrastry Git always reuses the application linked by its remote. Use `--remote` to
inspect something other than `origin`, `--branch` for a detached checkout or a
different destination branch, and `--json` for machine-readable activity. If more
than one application matches the repository and branch, resolve the duplicate
source associations in Infrastry before deploying.

## Tail logs

Applications may be selected by their immutable slug, full `team/app` ref, or
by a case-insensitive exact name:

```sh
infra logs customer-api
infra logs acme/customer-api --tail 500
infra logs "Customer API" --since 30m --dataset runtime
infra logs "Customer API" --component web --follow=false
infra logs "Customer API" --json | jq -r '.message'
infra logs                  # interactive application picker
```

The default view prints the newest 100 events and follows once per second.
Following uses an opaque server cursor, suppresses duplicates, refreshes OAuth
tokens automatically, and reconnects with bounded exponential backoff after a
temporary network or server failure. `--json` emits one JSON object per line.

Use `infra logs --help` for all filters.

## Development

```sh
go test ./...
go vet ./...
```

The test suite uses loopback-only HTTP servers to verify PKCE callback handling,
token rotation, authenticated retries, URL/query construction, app resolution,
and CLI output. No external service is required.

## Private application networking

You can connect to your Infrastry team's private network and resolve private addresses. 

```sh
infra auth login --super
infra network connect acme/customer-api
infra network status
infra network disconnect
```

`network connect` currently supports macOS. Keep it running in the foreground;
it asks sudo to install an interface, exact service routes, and private DNS.
Authentication and OS keychain access happen before elevation. Connections expire
after one hour. Ctrl-C, disconnect, or loss of authorization closes local access.
Gateway leases enforce revocation independently of editable client routes.

For a loopback port without elevation:

```sh
infra proxy acme/customer-api --service api --port 18080
infra db proxy acme/customer-api --database primary --port 15432
```

Omit `--port` to choose a free port. An explicitly occupied port is an error.
Use `--remote-port` when a service declares several ports.

To open an installed PostgreSQL (`psql`) or MySQL (`mysql`) client with automatic
login, approve database access once:

```sh
infra auth login --super
infra db connect acme/customer-api
```

The CLI selects the engine, username, and database name from the deployed
configuration and fetches that database's password through the authenticated API.
`db connect`, `db dump`, and `db proxy` select the database service automatically when
the application has exactly one. If it has several, the CLI lists their names;
choose one with `--database primary`. `--database` names the service;
`--database-name` optionally selects a different database inside it.
Owners/admins can use `--super` or `--scope database` to approve the permissions
for automatic login.

For a different database user, enter their password directly:

```sh
infra db connect acme/customer-api --database primary --user operator --password-prompt
```

`--password-prompt` works with `--scope network` and never retrieves a credential.
Use `db proxy` with your client's verified TLS configuration when custom TLS or
other client options are required. PostgreSQL and MySQL automatic login use the
[PostgreSQL password file](https://www.postgresql.org/docs/current/libpq-pgpass.html)
and a [MySQL option file](https://dev.mysql.com/doc/refman/8.4/en/option-files.html).

To save a database's schema and data to your computer, install PostgreSQL's
`pg_dump` or MySQL's `mysqldump` and run:

```sh
infra db dump acme/customer-api
infra db dump acme/customer-api --database primary --output ./backup.sql
```

Without `--output` (or `-o`), the CLI writes
`acme-customer-api-<UTC timestamp>.sql` in the current directory and prints its
path when finished. Dumps are plain SQL files with owner-only permissions.
Existing files are never overwritten. Failed or interrupted dumps are removed
and return a nonzero exit code; an abrupt process kill or computer crash can
leave an incomplete file.

`db dump` uses the same automatic login and private connection as `db connect`,
including `--database-name`, `--user`, `--password-prompt`, and `--remote-port`.
Use a `pg_dump` version at least as new as the PostgreSQL server's major version.
The [PostgreSQL dump](https://www.postgresql.org/docs/current/app-pgdump.html)
contains the selected database, excluding server-wide roles. The
[MySQL dump](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html) uses a single
transaction for InnoDB tables and includes schema, data, and triggers; stored
routines, scheduled events, and server-wide users are excluded. Avoid schema
changes during the dump. Each dump closes its private connection when finished.

One native/proxy connection runs per local CLI configuration at a time. Device
keys are separate for native and proxy modes and remain in the OS credential
manager. After removing a device in the web app, replace its revoked local key:

```sh
infra network reset-device acme --mode native
# For a proxy device, use --mode proxy instead.
```

On macOS, `Connected` is printed only after the system has loaded the application's
private DNS configuration and every service name resolves to its authorized
address. Setup waits up to 30 seconds; if DNS cannot become ready, the helper
removes its routes and resolver before the command reports failure.
