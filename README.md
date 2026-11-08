# kat-irc

A configurable Go IRC assistant with IRCv3 support, OpenAI Responses streaming,
ChatGPT OAuth or API-key authentication, and file-based persona and memory.
MIT licensed. No personal profile or deployment-specific configuration is bundled.

## Behavior

- Answers channel messages beginning with its nick followed by `:` or `@nick`
  followed by a non-nick character (whitespace or punctuation) or the end of the
  message. With nick `Kat`: `Kat: hello`, `Kat:hello`, `@Kat hello`, `@Kat:` and
  `@Kat, hi` work. Mid-sentence mentions, DMs and CTCP do not trigger replies.
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
  become context; addressed messages cause conversational inference. Optional
  reactions and channel-memory maintenance can also cause inference. `allowed_accounts`
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
- Makes one conversational inference request at a time, with a cooldown and timeout. IRC PING
  handling remains active while inference runs. Replies are sanitized and automatically split
  into UTF-8-safe components, using native multiline when negotiated. Failed/incomplete streams are not posted as answers.
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
available model identifiers. No inference is performed merely to validate setup.
If channel memory is enabled, its background maintenance can process retained
history after startup, according to its normal thresholds.

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
| `irc.multiline` | `auto` (default) negotiates native multiline and server limits; `off` forces plain messages |
| `irc.nick`, `irc.channels` | Bot identity, trigger prefix and channels to join; any other channel is left |
| `irc.tls` | Enable certificate-verified TLS, minimum TLS 1.2 |
| `irc.server_name`, `irc.ca_file` | Optional TLS name override and custom CA |
| `irc.password` | Optional IRC server password; fallback `IRC_PASSWORD` |
| `irc.user` | IRC username/ident (default `kat`); must be a valid ident (no `/` or `@`) |
| `irc.sasl_user`, `irc.sasl_password` | SASL PLAIN credentials; password fallback `IRC_SASL_PASSWORD` |
| `irc.allow_plaintext_auth` | Explicit opt-in to send IRC passwords without TLS |
| `openai.auth` | Exactly `chatgpt` or `api_key` |
| `openai.api_key`, `openai.credentials_file` | Private key or OAuth file location |
| `openai.model`, `openai.reasoning_effort` | Model and optional supported reasoning setting |
| `openai.web_search` | Offer hosted Web search for normal answers; off by default, never offered for spontaneous reaction checks |
| `openai.timeout_seconds` | Whole inference timeout, including credential refresh |
| `openai.max_output_tokens` | API-key output budget; omitted for direct ChatGPT OAuth |
| `bot.persona`, `bot.persona_file`, `bot.memory_file` | Inline instructions plus optional external files |
| `bot.history_messages`, `bot.history_file` | 1–200 recent messages per channel; empty path disables persistence |
| `bot.cooldown_seconds` | Minimum interval between conversational requests |
| `bot.max_reply_bytes` | Optional logical-answer UTF-8 byte budget, default 8192, range 512–65536; independent of server transport limits |
| `bot.max_reply_lines` | Legacy optional budget, 0–20; a positive value also caps answer bytes at `value × 350`; omit or set 0 for automatic transport sizing |
| `bot.typing` | Send IRCv3 typing indicators (`+typing`) while generating an answer |
| `bot.presence` | Add a live channel-membership summary (presence, away, account) to each request |
| `bot.reply_threading` | `model` (default), `always` or `never`; whether answers carry a `+reply` tag |
| `bot.allowed_accounts` | Optional IRC account allowlist; empty allows everyone in configured channels |
| `bot.reactions.enabled` | Offer the `react` tool, handle incoming reactions; off unless set |
| `bot.reactions.spontaneous` | Also evaluate ordinary messages for a reaction; requires `enabled` |
| `bot.reactions.min_interval_seconds` | Minimum gap between spontaneous evaluations (default 180, minimum 15) |
| `bot.images.enabled` | Offer the `image` tool: generate, upload and post an image URL; off unless set |
| `bot.images.model` | OpenAI image-generation model (required when enabled), e.g. `gpt-image-1` |
| `bot.images.api_key` | Platform API key for the Images API; required under ChatGPT OAuth, fallback `OPENAI_IMAGES_API_KEY` |
| `bot.images.size` | Requested image size (default `1024x1024`) |
| `bot.images.filehost` | Optional upload URL override; otherwise discovered from `soju.im/FILEHOST` |
| `bot.images.max_per_reply` | Images generated per model answer, 1–2 (default 1) |
| `bot.images.timeout_seconds` | Timeout for generation plus upload (default 120) |
| `bot.channel_memory` | Optional bounded, selective long-term memory learned from each channel; see below |
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
- `bot.reactions.spontaneous` adds one extra inference per interval: the model
  answers with a single emoji to react to the message that triggered the check, or
  `NONE` to skip. Raise `min_interval_seconds` to cut cost, or leave `spontaneous`
  false to react only alongside triggered answers.
- Each reaction the bot sends is appended to the conversation history, which keeps
  it from reacting twice to the same message.
- Logs: every sent reaction logs the emoji, and every spontaneous check logs its
  outcome (`reactions=1`/`0`, plus the model's answer). Inspect with
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

## Web search

Set `openai.web_search` to `true` in the private configuration file and restart
`serve`. This offers the hosted Responses `web_search` tool alongside the
existing tools. The model chooses when to search; this is not an unconditional
search for every message. Instructions encourage search for explicit requests,
current facts and uncertain factual questions. Spontaneous emoji checks never
receive this tool. The bot supplies the current UTC date on search-enabled turns.

Search can be available with either API-key auth or direct ChatGPT OAuth,
subject to the selected model and account/workspace policy. Check the current
[OpenAI direct-flow limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations).
Enabling the option does not establish account eligibility. Rejected requests
produce the normal service error; no API billing fallback or paid retry is made.
API-key search can incur tool-call and retrieved-token charges in addition to
inference. ChatGPT OAuth uses the account's plan limits.

Completed response annotations are converted to numbered references and
clickable source URLs in IRC. Source URLs share the logical answer byte budget;
room is reserved for them even if the prose is shortened. If the sources cannot
fit or a URL cannot be displayed safely in one component for plain-client
fallback, the bot returns an error instead of silently removing attribution. Search does
not download or execute arbitrary pages inside the pod. Queries are handled by
the hosted search service; avoid putting private information in public searches.

## Answer depth and model choice

The sample uses `gpt-6-luna` with low reasoning effort for short replies. For
more developed discussion, try `medium` reasoning, a persona that asks for
explanations when useful, and a larger transport budget. For example, merge
these settings into your existing configuration (preserving your auth and other
fields):

```json
{
  "openai": {
    "reasoning_effort": "medium",
    "timeout_seconds": 120,
    "max_output_tokens": 4096,
    "web_search": true
  },
  "bot": { "max_reply_bytes": 8192 }
}
```

`max_reply_bytes` limits the complete logical answer, including source and image
URLs; it is not a target to fill. Transport line and batch sizes are detected
from the server. Existing `max_reply_lines` settings remain accepted as a legacy
byte budget (16 means at most 5,600 bytes), but no longer represent the exact
number of physical IRC commands. Omit it or set it to 0 to use only the new
logical-answer budget. Higher budgets take longer to send because the bot paces
outbound commands to reduce flooding.
`max_output_tokens` is an API-key generation budget, including reasoning tokens;
it is intentionally omitted for ChatGPT OAuth. A higher reasoning effort can
increase latency and quota consumption. No exact response-time improvement is
guaranteed by a model change.

`gpt-6.1-terra` is another option to compare for writing quality; list the models
available to your account with `models` before changing `openai.model`. Model
availability, context limits and pricing are account/route dependent. A larger
context window does not by itself make answers deeper. Keep persona and memory
focused; they are supplied again with each request. OpenAI documents a 128k
search context limit even for models with larger native context windows.

To load your own persona and memory, set `bot.persona_file` to `persona.md` and
`bot.memory_file` to `memory.md`, then create those files next to `config.json`.
The pre-existing `memory.md` remains operator-maintained text. Optional learned
channel memory is separate; the bot never edits `persona.md` or `memory.md`.
Recent conversation is saved separately in `history.json`.
ChatGPT's saved memories and custom instructions are not imported by OAuth.

Persona, memory and the current channel history are sent to OpenAI for each
answer. Keeping them out of Git protects repository privacy, but does not make
prompt contents inaccessible to channel participants through model answers.
Only supply material appropriate for the channels in which the bot runs.

## Automatic IRCv3 multiline

By default, kat-irc requests `draft/multiline` alongside `batch` and
`message-tags`. It reads `max-bytes` and optional `max-lines` from CAP LS/NEW,
checks that the capability was ACKed, and refreshes the limits when capabilities
change. No server-size settings need to be copied into kat-irc. If support or
valid limits are missing, a capability is removed, or outgoing multiline is
rejected, it uses ordinary messages for subsequent answers. Use
`irc.multiline: "off"` to force plain transport for troubleshooting.

The complete answer preserves paragraphs and indentation. Wrapping a long
logical line uses `draft/multiline-concat`, so a capable client can reassemble it
without adding artificial line breaks. Components are sized using the IRC line
allowance, sender prefix, target and girc's own limit. Oversized answers are
packed into multiple batches within the advertised byte/line limits. A newline
between logical lines counts toward a batch's byte limit. Blank-only batches
are never emitted. Very small/invalid limits use plain transport instead.
Independent hard safety ceilings bound batch size and receive buffers; these
are not server limits users must synchronize manually.

Reply tags go on the opening BATCH, not its component messages. On echo-message,
the logical msgid and assembled text are tracked once for redaction. An answer
occupies one history/memory observation even if transport uses several batches.
Source/image URLs are kept whole where plain fallback permits; an unrenderable
answer is reported instead of silently dropping attribution. Outbound commands
remain paced; native multiline does not bypass server flood controls. A client
may display a batch only after its closing command, so grouping does not imply
instant display of the first fragment.

Live incoming multiline is assembled before trigger matching, account checks,
inference and history ingestion. Opening metadata (account, time, msgid) is
retained, and sender/target/account consistency is checked. Incomplete, expired,
oversized or malformed batches are discarded, never processed as partial
questions. Only top-level live batches are accepted; nested/history batches and
unknown references remain ignored to avoid replay-triggered inference. Buffers
are bounded by count and bytes, expired on batch traffic, and reset on reconnect.

Servers such as Ergo can deliver plain fallback to recipients that did not
negotiate multiline, including through a bouncer. That preserves readable text,
but does not guarantee a single grouped bubble, blank lines or logical msgid
semantics in every client. Actual Soju/Goguma/senpai support must be checked for
the deployed versions; the bot does not assume all IRCv3 clients support this
draft. See the [multiline specification](https://ircv3.net/specs/extensions/multiline).

## Selective long-term channel memory

Three layers can be supplied to a normal answer:

| Layer | File | Writer |
| --- | --- | --- |
| Persona and pre-existing long-term knowledge | `persona.md`, `memory.md` | Operator |
| Learned, selective long-term channel memory | `channel-memory.json` | Bot, through a bounded curation process |
| Exact recent messages | `history.json` | Existing bounded history buffer |

Enable learned memory by merging this into your existing `bot` configuration:

```json
"channel_memory": {
  "enabled": true,
  "file": "channel-memory.json",
  "max_bytes": 65536,
  "batch_messages": 40,
  "interval_seconds": 300
}
```

It is off by default. Relative paths resolve next to the configuration file, so
containers normally write `/data/channel-memory.json` on the existing PVC.
No new Kubernetes Secret, volume or deployment permission is required. The file
must be distinct from configuration, credentials, persona, fixed memory and
history; aliases to the same existing inode are rejected too.

All accepted live messages in watched channels are observed, including ordinary
conversation from participants outside the inference allowlist. Bot answers and
reaction/redaction context are observed too. Playback, DMs and unconfigured
channels are excluded by the same ingress rules as history. Enabling this means
ordinary channel conversation can lead to background inference, even if nobody
addresses the bot. The curator uses the configured model/auth, with low reasoning
and no tools. No API billing fallback is added. These extra calls consume the
account's quota or API budget; API-key curation uses an 8,192-token generation
ceiling, while OAuth omits the unsupported `max_output_tokens` field.

The curator receives the previous learned memory plus a batch of observations.
It returns a complete replacement: keep enduring events, useful explicitly
stated preferences, important decisions and recurring jokes; merge duplicates,
apply corrections and discard stale or lower-value details. Recent trivia must
not automatically displace an important old event. This is selective memory,
not an ever-growing archive or a summary that covers only the latest topic.
Attribution, dates, uncertainty and the difference between fact and fiction are
part of the curation instructions. Selection is model-generated and can make
mistakes; the byte bound is enforced by Go, not entrusted to the prompt.

`max_bytes` is a hard ceiling on the entire serialized UTF-8 JSON file, including
metadata and the bounded pending-observation queue. Accepted range: 4 KiB to
1 MiB, with enough room required for the configured channels. It is not a token
limit or a size the model must fill. Stored channels divide the available budget;
roughly half each channel's share is reserved for curated text, capped at 32 KiB
of JSON-encoded text per channel. The model is asked to stay below 75% of that
curated-text ceiling. JSON escaping counts toward the actual disk limit.

Each channel has its own memory and pending observations in the shared file.
Only that channel's curated text is included with its normal answers, as
untrusted user-role context rather than developer instructions. Curated text is
not included in spontaneous reaction checks. Removed channels stay on disk but
are not exposed to other channels or updated. Adding channels can require a
larger byte budget to preserve existing memories' allocated space.

The background worker checks every five seconds. A channel is eligible only
when it has observed at least `batch_messages` new entries (1–200) since its last
successful rewrite AND `interval_seconds` has elapsed since its last attempt
(minimum 30 seconds). Idle time and pending byte pressure never bypass the
message threshold. With the defaults, one isolated message causes no rewrite,
even after days of silence. The threshold counts accepted user messages, bot
answers and reaction/redaction context, not only messages addressed to the bot.

A persisted sequence checkpoint counts new observations independently of queue
retention, so a busy channel can still reach the threshold if the bounded buffer
has dropped some entries. Only buffered observations can enter the next rewrite;
dropped observations remain unavailable. Older files without a checkpoint are
migrated conservatively by counting their retained pending tail as new. A
threshold is a trigger, not an exact batch size: all currently buffered entries
are considered, and arrivals during a rewrite count toward the following batch. The worker starts only while conversational inference is idle, but
an answer may start while maintenance is already in flight. There is at most
one maintenance request in flight, separate from normal replies; PING handling
and new answers are not held until maintenance finishes.

Pending observations are independent of the short history window, so removing
an old entry from `history.json` does not normally remove it before curation.
They are checkpointed every ten seconds, on graceful shutdown, and immediately
before a model request. Each observation retains at most 4 KiB of content, or
less with a small/multi-channel byte budget. If traffic or an API outage fills
the bounded staging area, the oldest unprocessed observations are dropped and
a warning/counter records the loss. The file remains bounded even when the
model is unavailable. This cannot guarantee retention of every noteworthy event.

A valid replacement and removal of its processed observations are persisted in
one atomic rename with mode 0600. Messages arriving during curation remain pending
for a later pass. Invalid/oversized model output or a failed write preserves the
previous memory and batch; attempts are rate-limited rather than retried in a
tight paid loop. A malformed or oversized existing file is not overwritten:
startup waits, and `check` reports the problem. Lowering the limit below existing
state can require offline editing or increasing it again.

On first activation, only the messages still retained in `history.json` are
seeded, once. Already-discarded conversations cannot be recovered. After that,
learning covers newly observed messages while the bot is connected. To inspect
or edit the memory, stop the bot first. Clear a channel's entry to reset its
learned memory and pending batch. Old short-term history or future discussion
can mention forgotten facts again. The operator's `memory.md` is always separate.

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
