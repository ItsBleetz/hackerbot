# Hackerbot

[![CI](https://github.com/ItsBleetz/hackerbot/actions/workflows/ci.yml/badge.svg)](https://github.com/ItsBleetz/hackerbot/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/ItsBleetz/hackerbot)](https://github.com/ItsBleetz/hackerbot/releases/latest)

Hackerbot monitors the programs and reports available to a HackerOne researcher account and sends changes to Discord. It uses only HackerOne's documented Hacker API.

## What it does

- Retrieves every paginated program visible to the authenticated hacker, including accepted private programs.
- Retrieves each program's complete program object, structured scopes, and scope exclusions.
- Creates a silent baseline on the first successful run.
- Notifies Discord when a program is added or changed.
- Sends a concise, styled Discord overview first, then uploads one complete standalone HTML program report in a second ordered message.
- Places a visual divider between consecutive program notifications.
- For changed programs, names the changed parts in Discord and includes every full before/after value in the HTML.
- Includes the full policy, a HackerOne-style structured scope table, exclusions, researcher-specific program statistics, and raw API snapshot in the HTML without silent truncation. Scope assets are ordered Critical, High, Medium, Low, then None, with assets alphabetical inside each severity.
- Can be disabled completely, or monitors reports owned by the authenticated hacker for new reports, status changes, comments, and every new activity type returned by the Hacker API.
- Uses a separate Discord webhook for report notifications.
- Optionally keeps each report's events in one Discord Forum/Media thread.
- Stores current program and report snapshots transactionally in an embedded SQLite database.
- Supports an immediate `-handle` mode that does not modify the monitor baseline.
- Retries HackerOne and Discord rate limits and transient server failures.

The researcher API does not expose pending private invitations, program-page statistics, response metrics, bounty amounts, average bounties, thanks, updates, or collaborators. Hackerbot does not display a bounty table, call Program Management endpoints, or call HackerOne's private web GraphQL API.

Discord does not render attached HTML inline. Download/open the `<handle>-program-<UTC timestamp>.html` attachment in a browser. It is self-contained except for the optional program profile image loaded from HackerOne.

## Configuration

Copy `config.example.json` to `config.json` and `.env.example` to `.env`. By default, Hackerbot reads `.env` from the same directory as the selected config file. Keep both local files out of source control.

```text
HACKERONE_USERNAME=your_hackerone_username
HACKERONE_API_TOKEN=your_hackerone_api_token
DISCORD_PROGRAM_WEBHOOK=https://discord.com/api/webhooks/...
DISCORD_REPORT_WEBHOOK=https://discord.com/api/webhooks/...
HACKERBOT_REPORT_NOTIFICATION_MODE=summary
HACKERBOT_REPORTS_ENABLED=true
HACKERBOT_REPORT_NOTIFY_OWN_COMMENTS=false
HACKERBOT_REPORT_THREADS_ENABLED=false
```

Existing operating-system environment variables take precedence over values in `.env`. Use `-env C:\secure\hackerbot.env` to select another file; an explicitly selected missing or malformed file is an error.

Required:

| Environment variable | Purpose |
| --- | --- |
| `HACKERONE_USERNAME` | API token identifier (the Basic-auth username), not an email address |
| `HACKERONE_API_TOKEN` | Personal HackerOne API token |
| `DISCORD_PROGRAM_WEBHOOK` | Webhook for program notifications and `-handle` |

Optional:

| Environment variable | Purpose |
| --- | --- |
| `DISCORD_REPORT_WEBHOOK` | Enables report monitoring and notifications |
| `HACKERBOT_STATE_FILE` | Overrides the configured state-file location |
| `HACKERBOT_REPORT_NOTIFICATION_MODE` | Overrides `report_notification_mode` |
| `HACKERBOT_REPORTS_ENABLED` | `false` disables all report API reads and notifications |
| `HACKERBOT_REPORT_NOTIFY_OWN_COMMENTS` | `true` also sends comments authored by the report's researcher |
| `HACKERBOT_REPORT_THREADS_ENABLED` | `true` enables one Discord Forum/Media thread per report |

JSON settings:

| Setting | Default | Description |
| --- | --- | --- |
| `program_poll_interval` | `30m` | How often all programs and scopes are checked |
| `report_poll_interval` | `5m` | How often owned reports are checked |
| `request_interval` | `150ms` | Shared delay between HackerOne API reads |
| `report_request_interval` | `210ms` | Additional delay between report-list/detail reads |
| `scope_request_interval` | `1250ms` | Additional delay between structured-scope requests |
| `request_timeout` | `30s` | HTTP request timeout |
| `state_file` | `hackerbot.db` | SQLite database, relative to the config file |
| `reports_enabled` | `true` | Enables report polling when its webhook is configured; `false` makes no report API calls |
| `report_notification_mode` | `summary` | `summary` hides report titles and comment bodies; `detailed` includes them |
| `report_notify_own_comments` | `false` | Send comments whose actor is the report's researcher |
| `report_threads_enabled` | `false` | Put each report in its own thread; requires a Forum/Media-channel webhook |

The default delays stay below HackerOne's documented limits of 600 reads per minute and 50 structured-scope requests per minute. Hackerbot also applies a conservative report-read interval of 210ms (about 285 reads per minute). Configuration validation refuses values below 110ms, 210ms, and 1250ms for general, report, and scope reads respectively. Every retry passes through the same synchronized limiter, and the two schedulers share one limiter.

### Report notification modes

The default `summary` mode uses separate, clearly labeled event cards such as:

```text
### 🔄 Status changed
Report: #1231231
Status: Triaged → Informative

### 🟠 Severity changed
Report: #1231231
Severity: Medium → High

### 💬 New comment
Report: #1231231
Author: program-member
Content: Hidden in summary mode
```

Summary mode hides only the report title and comment body. It includes the report ID/link, previous and new status or severity, activity name, actor, and other activity values when HackerOne provides them. Each event gets its own Discord message for clarity. Unknown/new HackerOne activity types are still announced using a humanized form of their API activity type.

Set `report_notification_mode` to `detailed` for event-specific messages:

```text
### 🔄 Status changed
Report: #123
Title: Reflected XSS
Status: New → Triaged

### 💬 New comment
Report: #123
Title: Reflected XSS
Author: program-member

Comment
**Please retest** after clearing the cache.
```

A new report sends its ID and title. Status and severity changes show both the previous and new value. A new comment sends the ID, title, author when available, and the Hacker API's comment body as Discord Markdown. Other known or future activity types are sent with a readable activity name, actor when available, and their activity fields. No report JSON or report attachment is sent. Changing modes does not reset the baseline.

By default, a comment is suppressed when its activity actor matches the report's `reporter` relationship. Set `report_notify_own_comments` to `true` to include it. This comparison uses HackerOne resource IDs (and username as a fallback); it does not mistake the Basic-auth token identifier for a HackerOne username. If HackerOne omits actor/reporter identity from an activity, the bot cannot safely classify that comment as yours and sends it.

### Report threads

Set `report_threads_enabled` to `true` to keep each report's notifications together. The report webhook must belong to a Discord **Forum or Media channel**. The first event seen for a report creates a thread using Discord's webhook `thread_name` option; the returned thread/channel ID is saved inside that report's SQLite snapshot. Every later event is posted with `thread_id`.

The initial Hackerbot report baseline remains silent and creates no threads. Therefore, if an already-known report has no stored thread and later receives any event—including a state change—Hackerbot creates its thread with that event as the first message. A newly discovered report creates a thread containing its ID-and-title creation message.

A webhook in an ordinary text channel cannot create a thread by itself. Discord permits a webhook to post into an existing thread when its ID is already known, but creating a text-channel thread requires a bot-authenticated channel API operation, which Hackerbot intentionally does not request. Leave `report_threads_enabled` false for a normal text-channel webhook.

## Windows usage

Create the local files and edit `.env` with your actual secrets:

```powershell
Copy-Item .\config.example.json .\config.json
Copy-Item .\.env.example .\.env
notepad .\.env
```

Create the initial silent baseline and exit:

```powershell
.\hackerbot-windows-amd64.exe -config .\config.json -once
```

Run the internal schedulers continuously:

```powershell
.\hackerbot-windows-amd64.exe -config .\config.json
```

Send one program immediately without changing the baseline:

```powershell
.\hackerbot-windows-amd64.exe -config .\config.json -handle starbucks
```

This sends a snapshot embed and a file such as `starbucks-program-20260908T175906Z.html`. It does not change the stored baseline.

Run a bounded live Discord preview:

```powershell
.\hackerbot-windows-amd64.exe -config .\config.json -dummy
```

`-dummy` requests only the first three programs and first two reports returned by the Hacker API. It fetches the complete program/scope data for those three programs and the report details for those two reports, then sends them through the configured Discord webhooks. Programs are labeled as manual snapshots rather than new programs. Reports remain in HackerOne's returned order, and each report's existing activities are displayed oldest-to-newest without inventing state changes. The configured report notification mode, own-comment switch, and thread switch are respected.

The live preview does not open or change `hackerbot.db`, so it cannot initialize or affect either monitoring baseline. It is not an offline test: it performs rate-limited HackerOne `GET` requests and creates real Discord messages/attachments (and Forum/Media threads when enabled). Repeating it repeats those Discord messages.

To keep the secrets outside the application directory:

```powershell
.\hackerbot-windows-amd64.exe -config .\config.json -env C:\Secure\hackerbot.env
```

For a Windows server, run the continuous command under a dedicated service account using Task Scheduler or your normal service manager. Configure it to start at boot and restart after failure. Ensure its working directory and `-config` path are explicit.

## Linux usage

```bash
cp config.example.json config.json
cp .env.example .env
# Edit .env before running.

./hackerbot-linux-amd64 -config ./config.json -once
./hackerbot-linux-amd64 -config ./config.json
./hackerbot-linux-amd64 -config ./config.json -handle starbucks
./hackerbot-linux-amd64 -config ./config.json -dummy
```

## Building

Prebuilt Linux and Windows binaries, together with SHA-256 checksums, are available from the [latest GitHub release](https://github.com/ItsBleetz/hackerbot/releases/latest).

Building from source requires Go 1.25 or newer. The provided executables have no Go or SQLite runtime dependency.

Linux/macOS shell:

```bash
./build.sh
```

Windows PowerShell:

```powershell
.\build.ps1
```

Both scripts create:

- `dist/hackerbot-linux-amd64`
- `dist/hackerbot-windows-amd64.exe`

SQLite is embedded into each executable; the Windows server does not need SQLite, Go, GCC, or another runtime installed.

## First-run and failure behavior

Program and report baselines are independent. Each first successful check writes its baseline and sends no Discord notification. If `reports_enabled` is false or the report webhook is absent, report monitoring remains disabled and no report baseline is created.

State is stored in the `metadata`, `programs`, and `reports` tables in `hackerbot.db`. Program handles and report IDs are primary keys; each row contains the complete latest API snapshot as JSON. SQLite runs in WAL mode and all baseline updates use transactions.

After initialization, Hackerbot updates a changed snapshot only after Discord accepts the corresponding message. A failed webhook delivery is therefore retried during the next poll. An incomplete HackerOne collection fails the entire check so a temporary API error cannot look like removed programs.

Program notifications use two awaited Discord requests per program: the summary/change embeds are accepted first, then the timestamped HTML file is uploaded. A visual divider is sent before the next program in the batch. When several programs need notifications, handles are processed alphabetically. Inside the HTML, sections are ordered as overview, changes, guidelines, scope, exclusions, then raw API data.

A program missing from two consecutive complete collections is silently removed from the baseline. If access is later restored, it is treated as newly added. Hackerbot intentionally does not send program-removal notifications.

If the configured state path contains a state file from the older JSON version, Hackerbot automatically imports it into SQLite. The original is preserved next to it with a `.json-backup` suffix.

## Security notes

- Every request to HackerOne is constructed with HTTP `GET`; Hackerbot has no HackerOne create, update, comment, bounty, or delete method. It validates pagination and redirects so credentials cannot leave the configured API origin. The executable accepts only `https://api.hackerone.com` as that origin.
- Hackerbot additionally rejects every path outside `/v1/hackers/`; customer and Program Management endpoints cannot be called by this client.
- Hackerbot does write its own local SQLite state and uses HTTP `POST` only to deliver messages to the configured Discord webhooks.
- The SQLite database contains private program policies/scopes and owned report data. Protect the database, its `-wal`/`-shm` files, and any JSON migration backup as confidential.
- Program HTML attachments contain complete program information, including private-program policy and scope data. Send them only to an appropriately restricted Discord channel.
- `summary` report mode does not send report titles or comment bodies; it does send transition targets, actors, and other activity values. `detailed` additionally sends the title and relevant comment content, but never uploads full report JSON.
- Report thread IDs are stored in the private SQLite report snapshots. Thread creation/posting changes Discord only; HackerOne remains read-only.
- Hackerbot disables Discord mention parsing so program policy or report text cannot ping users or roles.
- Discord webhook URLs are secrets. Anyone holding one can post to its channel.
- Restrict `.env` so only the Hackerbot service account can read it. It contains the HackerOne API token and Discord webhook secrets.
- Do not run two Hackerbot processes against the same state file.
- Accepted private-program information remains subject to HackerOne's confidentiality requirements.

Official documentation:

- [Hacker API resources](https://api.hackerone.com/hacker-resources/)
- [Hacker API authentication, errors, and rate limits](https://api.hackerone.com/getting-started-hacker-api/)
- [Discord execute-webhook options (`thread_id` and `thread_name`)](https://docs.discord.com/developers/resources/webhook#execute-webhook)
- [Discord threads and webhook behavior](https://docs.discord.com/developers/topics/threads)
