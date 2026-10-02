# kat-irc

A configurable Go IRC assistant with IRCv3 support, OpenAI Responses streaming,
ChatGPT OAuth or API-key authentication, and file-based persona and memory.
MIT licensed. No personal profile or deployment-specific configuration is bundled.

## Behavior

- Answers channel messages beginning with its nick followed by `:` or `@nick`
  followed by whitespace/end of message. With nick `Kat`: `Kat: hello`, `Kat:hello`,
  and `@Kat hello` work. Mid-sentence mentions, DMs and CTCP do not trigger replies.
- Negotiates IRCv3 capabilities with girc, including account tags, server time,
  message tags and batches. Supports SASL PLAIN. Ignores playback batches and
  messages timestamped before the current connection.
- Keeps a bounded conversation history per channel. Ordinary channel messages
  become context; only addressed messages cause inference. `allowed_accounts`
  restricts inference to authenticated IRC accounts, not nicknames. It does not
  exclude other participants' messages from context.
- Makes one inference request at a time, with a cooldown and timeout. IRC PING
  handling remains active while inference runs. Replies are sanitized and split
  into bounded UTF-8 lines. Failed/incomplete streams are not posted as answers.
- Reconnects to IRC with backoff. Credentials, persona and memory never need to
  appear in Kubernetes manifests.

## Build and start

Requires Go 1.26.1 or newer and Linux/macOS (file locking uses `flock`).

```sh
go build -o kat-irc .
mkdir -m 700 data
cp config.example.json data/config.json
chmod 600 data/config.json
# Edit data/config.json: IRC endpoint, channels, auth and persona.
./kat-irc serve -config data/config.json
```

With missing/invalid configuration, referenced persona/memory files, or missing
credentials, `serve` stays alive and retries setup every two seconds. No `enabled`
switch or restart is needed for initial setup. The health server starts immediately:
`/healthz` returns 200; `/readyz` returns 503 until the bot has registered and joined
all configured channels. Readiness describes IRC availability, not OpenAI quota.

After the bot starts, restart it to apply changes to configuration, persona or
memory. OAuth credentials are reloaded before each request, so reauthorization
needs no restart. Use one running instance per configuration/data directory.

```sh
./kat-irc check -config data/config.json
./kat-irc models -config data/config.json
```

`check` validates configuration and referenced persona/memory files without a
network request. `models` requires configured credentials and lists the account's
available model identifiers. No automatic inference is performed at startup.

## Authentication

### Continue with ChatGPT

Set `openai.auth` to `chatgpt`, then in another terminal:

```sh
./kat-irc login -credentials data/oauth.json
```

Open the printed URL in your normal browser. The callback uses
`http://127.0.0.1:1455/auth/callback`. Complete consent; the terminal confirms the
result. No browser is launched by the bot. `-port` changes the callback port.

This implements OpenAI's documented direct flow for open-source clients: dynamic
client registration, PKCE, state/nonce checks and signed ID-token verification.
Plan use requires the `chatgpt.tokens.use.direct` permission and account eligibility.
It shares your ChatGPT plan's usage limits. Manage access and limits in ChatGPT
Settings. The available models and reasoning settings depend on your account.
There is **no automatic fallback to API billing**.

Tokens and the stable host identifier are stored beside the credentials file with
private file permissions. Refreshes are serialized across processes and rotated
credentials are saved atomically. `login` can be run again while `serve` is active;
requests may time out while login holds the credential lock. Choose another
credential path to connect another account, rather than overwriting an existing
account registration. Do not copy a live OAuth session to multiple active hosts.

The implementation follows the current [OpenAI registration guide](https://developers.openai.com/siwc/token-sharing-open-source/sign-in),
[models and inference guide](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference),
and [preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations).
The direct flow is a preview and may change. Automated tests use local mocks;
real account sign-in must be verified by the operator.

### API key

Set `openai.auth` to `api_key` and put the key in `openai.api_key` in your private
configuration file. An empty field can also use `OPENAI_API_KEY`. API usage has
separate usage-based billing; it is not included in a ChatGPT subscription.
An API key present in the file is ignored when auth is `chatgpt`.

## Configuration

Start from [config.example.json](config.example.json). All relative file paths
resolve beside the configuration file. The container reads `/data/config.json`.

| Setting | Purpose |
| --- | --- |
| `irc.server`, `irc.port` | Arbitrary IRC hostname/IP and port; no server is hardcoded |
| `irc.nick`, `irc.channels` | Bot identity, trigger prefix and channels to join |
| `irc.tls` | Enable certificate-verified TLS, minimum TLS 1.2 |
| `irc.server_name`, `irc.ca_file` | Optional TLS name override and custom CA |
| `irc.password` | Optional IRC server password; fallback `IRC_PASSWORD` |
| `irc.sasl_user`, `irc.sasl_password` | SASL PLAIN credentials; password fallback `IRC_SASL_PASSWORD` |
| `irc.allow_plaintext_auth` | Explicit opt-in to send IRC passwords without TLS |
| `openai.auth` | Exactly `chatgpt` or `api_key` |
| `openai.api_key`, `openai.credentials_file` | Private key or OAuth file location |
| `openai.model`, `openai.reasoning_effort` | Model and optional supported reasoning setting |
| `openai.timeout_seconds` | Whole inference timeout, including credential refresh |
| `openai.max_output_tokens` | API-key output budget; omitted for direct ChatGPT OAuth |
| `bot.persona`, `bot.persona_file`, `bot.memory_file` | Inline instructions plus optional external files |
| `bot.history_messages`, `bot.history_file` | 1–200 recent messages per channel; empty path disables persistence |
| `bot.cooldown_seconds`, `bot.max_reply_lines` | Minimum interval between requests and 1–20 output lines |
| `bot.allowed_accounts` | Optional IRC account allowlist; empty allows everyone in configured channels |

The sample selects `gpt-6-luna` with low reasoning effort as a starting point for
short conversational replies. Confirm availability using `models` in OAuth mode;
edit the configuration if the account exposes different identifiers. A persona
influences tone; it cannot guarantee identical behavior across models.

To load your own persona and memory, set `bot.persona_file` to `persona.md` and
`bot.memory_file` to `memory.md`, then create those files next to `config.json`.
Long-term memory is operator-maintained text; the bot does not automatically write
new facts into it. Recent conversation is saved separately in `history.json`.
ChatGPT's saved memories and custom instructions are not imported by OAuth.

Persona, memory and the current channel history are sent to OpenAI for each
answer. Keeping them out of Git protects repository privacy, but does not make
prompt contents inaccessible to channel participants through model answers.
Only supply material appropriate for the channels in which the bot runs.

## Containers and Kubernetes

```sh
docker build -t kat-irc:local .
# Ensure the directory is writable by UID/GID 10001 first.
docker run --rm --name kat-irc -v "$PWD/data:/data" kat-irc:local
```

The image runs as UID/GID 10001 and works with a read-only root filesystem. Mount
one writable volume at `/data`. It includes CA certificates, a shell and `tar`
for `kubectl exec` / `kubectl cp`. Only source files enter the Docker build context.

Use a Deployment with one replica and `strategy: Recreate`. Even one replica can
briefly have two pods during a default rolling update; Recreate stops the previous
revision first. This avoids duplicate IRC identities and competing PVC writers
during upgrades. There is a brief interruption during updates. It is not a
cluster-wide singleton guarantee; do not share the data between active replicas.

Configure liveness/startup probes on port 8080 `/healthz` and readiness on
`/readyz`. If installing with Helm, allow the release to install without waiting
for readiness while the operator sets up the PVC. Change/disable the health
listener with `serve -health-listen ADDRESS` (empty disables it).

No OAuth ingress, LoadBalancer or permanent callback Service is required. With
`POD` set to the running pod's name, start login in one terminal:

```sh
kubectl -n kat-irc exec -it "$POD" -- kat-irc login -credentials /data/oauth.json
```

Keep it running, then forward the callback in a second terminal:

```sh
kubectl -n kat-irc port-forward --address 127.0.0.1 "pod/$POD" 1455:1455
```

Open the URL printed by login **after** forwarding is active. The browser returns
to your workstation's loopback port and kubectl forwards it into the pod. Once
login succeeds, stop port-forward. If 1455 is occupied, use another matching port
on both sides and pass it to `login -port`.

An internal Kubernetes DNS name does not encrypt IRC traffic. Plain IRC can work
inside a trusted cluster with `irc.tls=false`; choose TLS where transport privacy
is required. HTTPS to OpenAI is always certificate-verified independently.

## Development and releases

```sh
go test -race ./...
go vet ./...
go build ./...
```

Tests use a local TCP IRC server and HTTP mocks: negotiation, trigger/account/replay
filtering, PING during inference, output sanitation, history isolation, setup
waiting, health states and serialized OAuth refresh. They do not exercise a real
OpenAI account or a Kubernetes cluster.

GitHub Actions tests and builds the container on PRs. On `main`, it publishes
`ghcr.io/<owner>/<repository>:<VERSION>` and a commit SHA tag with `GITHUB_TOKEN`.
Increase `VERSION` for subsequent releases. Set the GHCR package visibility to
public before anonymous cluster pulls. The Docker build context uses an allowlist;
keep all personal/runtime files in `data/`, outside source control.

## License

[MIT](LICENSE).
