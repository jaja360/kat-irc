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
- Threading is controlled by `bot.reply_threading`. With `model` (the default)
  the model may prefix an answer with `[[reply:<msgid>]]` to mark it as an IRCv3
  reply (`+reply`) to one earlier message when answering a specific person; the
  marker is stripped before posting. Otherwise the answer addresses the channel.
  `always` threads every answer to the triggering message; `never` disables
  threading. Clients that render replies (for example goguma) show the question
  inline. With `bot.typing`, it also sends `+typing=active` while generating and
  `+typing=done` once the answer starts.
- Keeps a bounded conversation history per channel. Ordinary channel messages
  become context; only addressed messages cause inference. `allowed_accounts`
  restricts inference to authenticated IRC accounts, not nicknames. It does not
  exclude other participants' messages from context.
- Sees IRCv3 reactions: incoming `TAGMSG` with `+draft/react` and a `+reply`
  target is recorded as context, so the bot knows who reacted to which message
  (the older `+draft/reply` target tag is still accepted). A later
  `+draft/unreact` is recorded too, so a removed reaction does not leave stale
  context behind.
- With `bot.presence`, each request also receives a bounded, live summary of the
  channel's members from girc's IRCv3 state (NAMES, join/part, `away-notify`,
  `account-notify`, `extended-join`), so the model can tell who is present, away
  or authenticated. The summary is injected as instructions and never written to
  the conversation history.
- Can react back with emoji. When `reactions.enabled` is set, the model gets a
  `react` tool and may attach `+draft/react` + `+reply` client-only tags to
  any message it saw in the conversation, not only the latest one. Message ids are
  exposed to the model as a `[msgid:…]` prefix; ids that were not actually seen in
  that channel are dropped, so a hallucinated target never becomes a stray TAGMSG.
  A reaction-only answer sends no chat text.
- Reacts spontaneously when `reactions.spontaneous` is set: ordinary,
  non-triggering messages get a reaction-only evaluation at most once every
  `reactions.min_interval_seconds`, and never while a real answer is in flight.
  This bounds the extra inference cost; the model decides whether a reaction is
  warranted and may do nothing.
- With `bot.redaction`, the model gets a `redact` tool and can retract one of its
  own recent messages (IRCv3 `draft/message-redaction`, sent as `REDACT`). Only
  msgids of messages the bot actually sent, learned from `echo-message`, are
  accepted, and at most one per answer; the tool is not offered unless the server
  negotiates both capabilities, so it fails closed.
- With `bot.images`, the model also gets an `image` tool: it can generate an
  image with a separate OpenAI image model, upload the bytes to the server's
  `soju.im/FILEHOST` HTTP upload host, and post the resulting URL. The image is
  produced by the configured image model, not by the chat model.
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
| `irc.user` | IRC username/ident (default `kat`); must be a valid ident (no `/` or `@`) |
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
| `bot.typing` | Send IRCv3 typing indicators (`+typing`) while generating an answer |
| `bot.presence` | Add a live channel-membership summary (presence, away, account) to each request |
| `bot.reply_threading` | `model` (default), `always` or `never`; whether answers carry a `+reply` tag |
| `bot.allowed_accounts` | Optional IRC account allowlist; empty allows everyone in configured channels |
| `bot.reactions.enabled` | Offer the `react` tool, handle incoming reactions; off unless set |
| `bot.reactions.spontaneous` | Also evaluate ordinary messages for a reaction; requires `enabled` |
| `bot.reactions.min_interval_seconds` | Minimum gap between spontaneous evaluations (default 180, minimum 15) |
| `bot.reactions.max_per_reply` | Reactions emitted per model answer, 1–5 (default 2) |
| `bot.images.enabled` | Offer the `image` tool: generate, upload and post an image URL; off unless set |
| `bot.images.model` | OpenAI image-generation model (required when enabled), e.g. `gpt-image-1` |
| `bot.images.api_key` | Platform API key for the Images API; required under ChatGPT OAuth, fallback `OPENAI_IMAGES_API_KEY` |
| `bot.images.size` | Requested image size (default `1024x1024`) |
| `bot.images.filehost` | Optional upload URL override; otherwise discovered from `soju.im/FILEHOST` |
| `bot.images.max_per_reply` | Images generated per model answer, 1–2 (default 1) |
| `bot.images.timeout_seconds` | Timeout for generation plus upload (default 120) |
| `bot.redaction` | Offer the `redact` tool so the model can retract its own messages; off unless set |

### Reactions

Reactions are IRCv3 client-only tags: the bot sends
`@+draft/react=<emoji>;+reply=<msgid> TAGMSG <channel>` and reads the same tags on
incoming `TAGMSG` (including `+draft/unreact` when a user removes a reaction), so
the reaction target uses the ratified `+reply` tag. Older clients that send
`+draft/reply` as the target are still understood. Requirements and caveats:

- The IRC server must relay client-only tags and message ids, and the bot must
  negotiate `message-tags` (it does by default). Ergo does this; soju relays
  client-only tags too. Servers without it will simply deliver no reactions, and
  the bot logs that it cannot react rather than failing.
- Reactions target a message id, so the target message must still be inside the
  per-channel history window (`bot.history_messages`). Reacting to something
  further back is not possible.
- `bot.reactions.spontaneous` adds one extra inference per interval, even when the
  result is "no reaction". Raise `min_interval_seconds` to cut cost, or leave
  `spontaneous` false to react only alongside triggered answers.
- Each reaction the bot sends is appended to the conversation history, which keeps
  it from reacting twice to the same message.
- Logs: every sent reaction logs the model's `reason`, and every spontaneous check
  logs its outcome explicitly, including `reactions=0` with the model's one-sentence
  note. Both are logs only: the note is never sent to IRC and never stored in
  `history.json`. Inspect with
  `kubectl -n kat-irc logs deploy/<deployment> | grep -E 'reacted|reaction check'`.
  As a consequence, the pod log contains short paraphrases of channel content.

### Images

`bot.images.enabled` offers the model an `image` tool. When it is called, Kat
generates the image through the OpenAI Images API with the model named by
`bot.images.model`, decodes the returned image data, uploads the bytes to the
server's HTTP upload host, and posts the resulting URL. Requirements and caveats:

- The image is produced by `bot.images.model` (an image model such as
  `gpt-image-1`), not by the chat model; the chat model only decides to call the
  tool. Confirm the identifier with `models` and your account's access.
- Image generation is **not supported by the ChatGPT-plan OAuth flow** (OpenAI
  lists image generation as an unsupported tool for that flow). It therefore needs
  a Platform API key: set `bot.images.api_key` (or `OPENAI_IMAGES_API_KEY`) to keep
  chat on ChatGPT OAuth, or set `openai.auth` to `api_key` to use the shared key for
  both. Config validation rejects `images.enabled` under ChatGPT auth with no key.
- Upload follows the soju `soju.im/FILEHOST` extension: a raw-body HTTP `POST`
  carrying the image bytes with a `Content-Disposition` filename, authenticated
  with HTTP Basic using `irc.sasl_user` / `irc.sasl_password` when set, otherwise
  `irc.user` / `irc.password`. On success the `Location` header is resolved against
  the upload URL and posted.
- Through a soju bouncer, use SASL PLAIN with `irc.sasl_user` set to
  `<soju-user>/<network>` (soju selects the upstream network from the SASL
  username). The bot suppresses girc's duplicate `AUTHENTICATE` after soju's
  `CAP NEW` (cap-notify), which soju would otherwise treat as upstream SASL and
  forward to the network, causing a reconnect loop. The file host's HTTP Basic
  auth reuses the same credentials.
- The upload host is discovered from the `soju.im/FILEHOST` (soju) or
  `draft/FILEHOST` (Ergo) ISUPPORT token, or set with `bot.images.filehost`, and
  the advertised value is logged at startup. If none is present the tool fails
  closed, logs it, and posts a short failure message instead of staying silent;
  over TLS a plaintext upload host is refused. Note that a server advertises a
  host only if it has one: Ergo requires an `additional-isupport` entry pointing
  at an external upload server, and soju only advertises while it is the server
  the bot is connected to.
- Images live on the file host: they are public to anyone with the URL, outlive
  the conversation, and image generation is billed like any other image request.

### Redaction

`bot.redaction` offers the model a `redact` tool so it can retract one of its own
messages (a user criticising the bot's message can prompt this). It requires the
server to support `draft/message-redaction` and `echo-message`; the bot requests
both only when redaction is enabled, and the tool is withheld when either is
missing. Guardrails:

- Only msgids of messages Kat itself sent are accepted. They are learned from
  `echo-message` and kept in a bounded per-channel list, so the model cannot
  redact another user's message or invent a target.
- At most one message is retracted per answer, and only when a message is being
  addressed to the bot (`allowed_accounts` still applies to triggers).
- Redaction is permanent; clients that understand the extension show a tombstone.
- soju passes `draft/message-redaction` through only when the upstream network
  supports it, so the tool may simply be unavailable on some networks.

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
# Ensure the directory is writable by UID/GID 568 first.
docker run --rm --name kat-irc -v "$PWD/data:/data" kat-irc:local
```

The image runs as UID/GID 568 and works with a read-only root filesystem. Mount
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
waiting, health states, serialized OAuth refresh, SSE function-call parsing,
reaction send/receive, spontaneous throttling and log-note bounds. They do not
exercise a real OpenAI account or a Kubernetes cluster.

GitHub Actions tests and builds the container on PRs. On `main`, it publishes
`ghcr.io/<owner>/<repository>:<VERSION>` and a commit SHA tag with `GITHUB_TOKEN`.
Increase `VERSION` for subsequent releases. Set the GHCR package visibility to
public before anonymous cluster pulls. The Docker build context uses an allowlist;
keep all personal/runtime files in `data/`, outside source control.

## License

[MIT](LICENSE).
