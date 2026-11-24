# tikker User Guide

This guide explains how to run tikker after deployment: the first-run setup wizard, logging in to the web panel,
adding a router and its channels, sending SMS messages, using the inbox, API tokens, users and roles, the audit log,
and how to back up the data. The end of the guide has REST API examples (curl, Python, PHP) and frequently asked
questions.

Deployment is described in [docs/deploy-mikrotik.md](deploy-mikrotik.md) (container on the router) and
[docs/deploy-server.md](deploy-server.md) (Docker Compose, systemd). The Polish version of this guide is
[user-guide.md](user-guide.md), and the two should stay in sync.

Panel page addresses are relative to the instance address, for example `https://sms.example.com/devices`.

> **Configuration.** Every setting has a key in the YAML file and a `TIKKER_*` variable (for example `listen` is
> `TIKKER_LISTEN`, and `setup_secret` is `TIKKER_SETUP_SECRET`). Settings are passed in the process environment or in
> the file given by `--config`. `tikker --check-config` prints the effective settings with secrets masked, and exits
> with code 2 when something is wrong.

## 1. First run

There are two ways to create the administrator account. Choose one.

**A. Password in the `TIKKER_ADMIN_PASSWORD` variable.** When the database is empty, tikker creates a user named
`admin` with this password at startup (at least 12 characters). After the first login, change the password in the panel. When the database is not empty, the variable is ignored,
but do not just remove it: while the listen address is outside loopback, either `TIKKER_ADMIN_PASSWORD` or
`TIKKER_SETUP_SECRET` must stay set (see below).

**B. Setup wizard in the browser (`/setup`).** As long as no user exists, the login page redirects to `/setup`.
The form asks for:

- a user name,
- the setup secret (the field appears only when `TIKKER_SETUP_SECRET` is set),
- a password (at least 12 characters) and its confirmation.

After you submit, the account is created and you are signed in. The wizard disappears as soon as at least one user
exists.

**Setup secret.** The default listen address `:8080` covers all interfaces. If the listen address is outside loopback,
tikker will not start unless `TIKKER_ADMIN_PASSWORD` or `TIKKER_SETUP_SECRET` is set (at least 12 characters). This
is checked on every start, even when users already exist, so **do not remove both variables**. It keeps anyone on
the network from taking over an uninitialized instance. Recommended: set `TIKKER_SETUP_SECRET` (you type it into the
wizard; once a user exists the wizard is disabled, so the secret no longer unlocks anything) and leave
`TIKKER_ADMIN_PASSWORD` unset. Alternative: listen on `127.0.0.1` behind a reverse proxy, where neither variable is
required.

## 2. Logging in and sessions

- Login page: `/login`. Enter your user name and password.
- A session expires after 1 hour of inactivity or 12 hours after login. You can change these with
  `TIKKER_SESSION_IDLE_TIMEOUT` and `TIKKER_SESSION_ABSOLUTE_TIMEOUT`.
- Login attempt limits are fixed: per user name, 5 attempts and then one per minute; per IP address, 20 attempts and
  then one every 6 seconds. After too many attempts, login returns an error and you have to wait.
- Logout: the button in the panel (`POST /logout`).
- Set the public address in `TIKKER_BASE_URL`. Additional origins that may send state-changing requests (for example a
  different panel address) go in `TIKKER_TRUSTED_ORIGINS`.
- Behind a reverse proxy (for example Caddy), set `TIKKER_TRUSTED_PROXIES` to the proxy address. tikker then trusts the
  `X-Forwarded-For` header only from that proxy. Without this, login limits and the audit log see the proxy address
  instead of the client.

Panel menu: **Dashboard**, **Messages**, **Send** (operator and admin), **Devices**, **API tokens**, **Users**, and
**Audit log** (the last four for admins only). Every user has a **My profile and password** page where they change
their password.

## 3. Devices and channels

A device is a MikroTik router. A channel is an LTE port (a modem with a SIM card) on a device, for example `lte1`.
Messages go out through channels. Only an administrator manages devices and channels.

### Adding a device

Panel: **Devices → New device**, or `POST /api/v1/devices`. There are two connectors:

- **`routeros-rest`** (default). tikker connects to the router's REST API.
  - `base_url`: the address without `/rest`, for example `https://10.20.0.1`. Plain `http://` is allowed only for
    addresses listed in `TIKKER_ALLOWED_HTTP_HOSTS`.
  - `username` and `password`: the API account on the router (see `deploy/routeros/setup.rsc`).
  - `tls_mode`: `verify` (certificate checked against system CAs, default), `ca` (your own CA in `ca_pem`),
    `fingerprint` (a pinned SHA-256 fingerprint, typical for the self-signed certificate of RouterOS), or `insecure`
    (no verification; the panel shows a warning).
- **`agent`**. The router connects to tikker itself, so it works behind CGNAT. You provide `agent_token` (at least 32
  characters, unique for each device). Details: [agent.md](agent.md).

After adding a device, run **Test** (`POST /api/v1/devices/{id}/test`). The password and agent token are never
returned by the API.

### Adding a channel

On the device page, add a channel: `port` (for example `lte1`), optionally a name, tags, limits, and an enabled
switch. Defaults:

| Field | Default | Range | Meaning |
|---|---|---|---|
| `rate_per_min` | 6 | 1–600 | Sending rate limit of the channel. Excess messages wait in the queue; this is not a `429` error. |
| `max_segments` | 3 | 1–10 | Maximum number of segments of one message. Works together with the connector's capabilities. |
| `transliterate` | `true` for `routeros-rest`, `false` for `agent` (only because `TIKKER_AGENT_UCS2` defaults to `true`; for a connector without UCS-2 the server picks `true`) | – | Replaces Polish letters with ASCII before sending (see section 4). |
| `tags` | empty | up to 16 tags | Routing: a request with `tags` goes to a channel that has **all** of those tags. |

A channel state is `ok`, `degraded`, `down`, or `disabled`. Health is checked every 60 s (`TIKKER_HEALTH_INTERVAL`).
Disabling a channel with the switch in the panel takes precedence over the health check result.

> **Note on RouterOS.** The `routeros-rest` connector assumes GSM-7 only, one segment, and no concatenation. These
> assumptions come from research (`ASSUMPTION(T00)`) and have not yet been confirmed on hardware. Check them on your
> model before running in production.

## 4. Sending SMS messages

### Panel

**Send** shows the form. Before sending, you can click the preview (`POST /send/preview`), which shows the number of
parts and segments, and the characters that transliteration would replace.

### Recipient number

`to` is normalized to E.164 using the default region from `TIKKER_DEFAULT_REGION` (default `PL`). All of these are the
same number: `+48600100200`, `+48 600 100 200`, `600100200`. Short and alphanumeric numbers work only when
`TIKKER_ALLOW_SHORT_NUMBERS=true` is set. An invalid number returns `422` (`invalid_phone_number`).

### Body, encoding and segments

The body is UTF-8 text of 1–1600 characters. tikker chooses the encoding itself:

| | GSM-7 | UCS-2 |
|---|---|---|
| When | the text contains only characters from the GSM 03.38 alphabet (for example `@`, `€`, `é`, `ü`, and letters without Polish diacritics) | at least one character outside GSM-7 is present, for example `ą ć ę ł ń ó ś ź ż` |
| One SMS | 160 characters | 70 characters |
| Each segment when there are several | 153 characters | 67 characters |

GSM-7 extension characters (`^ { } \ [ ] ~ | €`) take **two** positions in an SMS, so they count double.
A single Polish letter switches the whole SMS to UCS-2, which lowers the limit from 160 to 70 characters.

Example: `Zażółć gęślą jaźń` has 17 characters, but it needs UCS-2, so it fits in one SMS (70 characters).

### Transliteration

When a channel has `transliterate=true`, tikker replaces characters outside GSM-7 with ASCII before counting
segments: `ą→a, ć→c, ę→e, ł→l, ń→n, ó→o, ś→s, ź→z, ż→z`. Characters with no equivalent (for example emoji or CJK
characters) become `?`. The preview in the panel shows which characters will be replaced.

When transliteration is on, the stored body (the `body` field in the history and in API responses) is already the
transliterated version, and `encoding` and `segments` refer to that version. For the example above, transliteration
gives `Zazolc gesla jazn`: GSM-7 text that fits in one SMS (160 characters).

For a channel without transliteration and without UCS-2 support, text with Polish letters returns `422`
(`unsupported_characters`). The `routeros-rest` connector does not support UCS-2, so transliteration is on by default
for it.

### Long messages

The number of segments must not exceed the effective limit, which is the smaller of the channel's `max_segments` and
the connector's capabilities. What happens when the limit is exceeded is set by `TIKKER_LONG_MESSAGE_STRATEGY` (a global setting, not per channel):

- `reject` (default): `422` (`too_many_segments`); the message is not queued;
- `split`: the text is split into several independent messages with a `(1/N) ` prefix, each with its own status.
  The total number of segments of all parts still cannot exceed the channel's `max_segments`; beyond that you get
  `422` (`too_many_segments`) as well;
- `truncate`: the text is cut to the limit.

### Message life cycle

`queued` → `sending` → `sent` or `failed`. A queued message can be cancelled (`queued` → `cancelled`), either in the
panel or with `DELETE /api/v1/messages/{id}`; for other states the API returns `409`. `sent` means the router accepted
the SMS. tikker receives no delivery reports, so there is no "delivered" state.

## 5. Messages and inbox

**Messages** lists outgoing and incoming SMS messages, newest first. Filters: direction (`in`/`out`), status, number,
channel, and date range. The same list is available in the API (`GET /api/v1/messages`).

**Inbox.** Incoming SMS messages have direction `in` and status `received`. tikker polls the inbox of each channel
every 15 s (`TIKKER_INBOX_POLL_INTERVAL`), so a new SMS appears within that time at most.

**Retention.** Message bodies are deleted after `TIKKER_RETENTION` (default `2160h`, that is 90 days). The record stays,
and its `body` becomes `null`.

## 6. API tokens

A token is access to the API. Create it in the panel (**API tokens**, admins only), or from the command line on the same
instance (with the same `TIKKER_DATA_DIR`):

```sh
tikker token create --user admin --name ci --scope sms:send --scope sms:read --ttl 720h
```

Defaults: owner `admin`, name `cli`, scope `admin`, no expiry (`--ttl 0`). The token, in the form
`tkr_<prefix>_<secret>`, is shown **only once**. In a container: `docker compose exec tikker /tikker token create ...`. On the MikroTik container (a `scratch` image
with no shell) the CLI is not available: create the token in the panel or with `POST /api/v1/tokens`.

| Scope | Allows |
|---|---|
| `sms:read` | reading messages and channels |
| `sms:send` | sending and cancelling messages |
| `admin` | managing devices, channels, tokens and users; includes `sms:read` and `sms:send` |

The effective permissions of a token are the intersection of its scopes and the role of its owner. A token whose owner
has the `viewer` role cannot send an SMS, even with the `sms:send` scope. A token of a disabled user, or an expired or revoked
token, is rejected with `401`, and a missing scope gives `403`.

Good practice: one token per integration, with the narrowest scope (for example, a ticketing system gets only
`sms:send`), and with an expiry date. To revoke a token, delete it in the panel or with `DELETE /api/v1/tokens/{id}`.

## 7. Users and roles

Administrators manage users (**Users**). Each user has one of these roles:

| Role | Can |
|---|---|
| `viewer` | view the dashboard and messages |
| `operator` | as `viewer`, plus send SMS messages and cancel queued messages |
| `admin` | everything: devices, channels, tokens, users, webhooks, audit |

A user can be disabled (their tokens stop working). The password is changed on the **My profile and password** page. It is recommended
to give each person their own administrator account instead of sharing one login.

## 8. Audit log

**Audit log** (admins only) shows events, filterable by action and actor. Recorded events include, among others: successful
and failed logins, logouts, creation and changes of users, creation and deletion of tokens, devices and channels,
sending and cancelling messages, and changes to webhooks. The audit log is available only in the panel, not in the API.

## 9. Webhooks, metrics and the agent connector

- **Webhooks** (`/api/v1/webhooks`, `admin` scope): tikker sends a `POST` when an SMS arrives (`incoming_sms`) and when
  an outgoing message changes to `sent` or `failed` (`message_status`). Details and the HMAC signature:
  [webhooks.md](webhooks.md).
- **Prometheus metrics**: `GET /metrics`, no token required. The endpoint needs no authentication, so on a public
  address restrict it at the reverse proxy to your monitoring network. See [metrics.md](metrics.md).
- **`agent` connector**: the router polls tikker over HTTPS, with no VPN and no port forwarding: [agent.md](agent.md).
- **Threat model**: [security/threat-model.md](security/threat-model.md).

## 10. Backing up `/data`

The data directory (`/data` in the container, `/var/lib/tikker` in a server deployment) contains:

- `tikker.db` together with its WAL files (`tikker.db-wal`, `tikker.db-shm`): the whole database, that is devices,
  messages, tokens, users, and the audit log;
- `secret.key`: the AES-256-GCM key that encrypts router passwords and agent tokens.

Back up **both** files together and protect them like secrets. The database alone cannot decrypt the router passwords,
and a copy of `secret.key` is essential to restore the instance. A key given through `TIKKER_SECRET_KEY` (32 bytes in
base64) works the same way as the file. If you change or lose `secret.key` while the database contains devices,
startup fails with a key mismatch error and the panel is unreachable. Restore the original key from a backup (or
remove the devices from the database). With a database that has no devices, a missing key is silently regenerated.

A safe backup: stop tikker, copy the data directory, start tikker again. An example for Docker Compose is in
[deploy-server.md](deploy-server.md#7-kopie-zapasowe) (section 7 is in Polish), and for the container on MikroTik in
[deploy-mikrotik.md](deploy-mikrotik.md#8-kopia-zapasowa-data) (section 8 is in Polish). To restore: stop the service,
replace the files, and start it again, keeping the file owner (`65532:65532` in the container).

## 11. REST API: examples

Base address: `https://<instance>/api/v1`. Every request needs the header `Authorization: Bearer tkr_…`.
The full contract: [api/openapi.yaml](../api/openapi.yaml).

| Purpose | Method and path | Scope |
|---|---|---|
| Send an SMS | `POST /api/v1/messages` (`Idempotency-Key` header optional but recommended) | `sms:send` |
| Message status | `GET /api/v1/messages/{id}` | `sms:read` |
| Incoming SMS messages | `GET /api/v1/messages?direction=in` | `sms:read` |
| Cancel | `DELETE /api/v1/messages/{id}` (only in `queued` state) | `sms:send` |

**Sending.** Body (`application/json`): `to` (required), `body` (required), optionally `client_ref` (up to 128
characters, returned in the response), `tags`, `channel_id`, and `expires_at` (RFC 3339, in the future). The response is
`202 Accepted`, the `Location` header points to the first message, and `messages[]` contains the created messages in
the `queued` state. There is usually one; with `split` there are more. To send many messages at once (up to 100, atomically) use
`POST /api/v1/messages/batch`; `Idempotency-Key` then applies to the whole batch (details in `api/openapi.yaml`).

**Idempotency.** `Idempotency-Key` (1–255 printable ASCII characters, no spaces, for example a UUID v4) is valid for 24 h per token:

- same key and same body: the same `202` response with the same messages and the header `Idempotent-Replayed: true`,
  with no new message;
- same key with a different body: `422` (`idempotency-key-mismatch`);
- same key while the earlier request is still running: `409` (`idempotency-key-in-use`).

When you do not know the result (a timeout, a network error), send the request again with the **same** key. Do not
generate a new one.

**Errors.** Error responses are `application/problem+json` (RFC 9457). The `type` field has the form
`urn:tikker:problem:<code>`, and for `422` the `errors[]` array gives the field and the code, for example
`too_many_segments`.

**Limits.** Every request with a valid token uses up the token's limit: by default 120 requests per 60 s
(`TIKKER_API_RATE_LIMIT`, `0` turns the limit off). The headers `RateLimit-Limit`, `RateLimit-Remaining` and
`RateLimit-Reset` show the state; once the limit is exhausted you get `429` with `Retry-After`.

### curl

```sh
TIKKER_URL=https://sms.example.com/api/v1
TOKEN='tkr_xxxxxxxx_yyyyyyyy'   # token from section 6, scopes sms:send and sms:read

# 1. Send with an idempotency key
KEY=$(openssl rand -hex 16)
curl -sS -X POST "$TIKKER_URL/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"to":"+48600100200","body":"Twój kod weryfikacyjny: 482913","client_ref":"otp-7f3a91"}'
# -> {"messages":[{"id":"01M89...","status":"queued",...}]}

# 2. Message status (id from the response)
MSG_ID=01M89304ZQ61F6CVNX486V10VC
curl -sS "$TIKKER_URL/messages/$MSG_ID" -H "Authorization: Bearer $TOKEN"

# 3. Incoming SMS messages (newest first, 50 per page)
curl -sS "$TIKKER_URL/messages?direction=in&limit=50" -H "Authorization: Bearer $TOKEN"
# next page: add &cursor=<next_cursor> until next_cursor is null
```

### Python (stdlib, no dependencies)

```python
import json
import os
import urllib.error
import urllib.parse
import urllib.request
import uuid

BASE = os.environ.get("TIKKER_URL", "https://sms.example.com/api/v1")
TOKEN = os.environ["TIKKER_TOKEN"]


def call(method, path, body=None, headers=None):
    data = None
    req_headers = {"Authorization": "Bearer " + TOKEN, "Accept": "application/json"}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        req_headers["Content-Type"] = "application/json"
    req_headers.update(headers or {})
    req = urllib.request.Request(BASE + path, data=data, method=method, headers=req_headers)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.loads(resp.read() or b"null")
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"HTTP {e.code}: {e.read().decode('utf-8', 'replace')}") from e


# 1. Send; store the key together with the request so you can repeat it after a network error
key = str(uuid.uuid4())
sent = call(
    "POST",
    "/messages",
    {"to": "+48600100200", "body": "Twój kod weryfikacyjny: 482913", "client_ref": "otp-7f3a91"},
    {"Idempotency-Key": key},
)
msg_id = sent["messages"][0]["id"]

# 2. Status
msg = call("GET", f"/messages/{msg_id}")
print(msg["status"], msg["last_error"])

# 3. Incoming, all pages
cursor = None
while True:
    query = {"direction": "in", "limit": 50}
    if cursor:
        query["cursor"] = cursor
    page = call("GET", "/messages?" + urllib.parse.urlencode(query))
    for m in page["items"]:
        print(m["created_at"], m["peer"], m["body"])
    cursor = page["next_cursor"]
    if cursor is None:
        break
```

Run it with: `export TIKKER_TOKEN="$(cat ~/.config/tikker/token)"` (or fetch the token from a secret store, or use
`read -rs TIKKER_TOKEN && export TIKKER_TOKEN`), then `python3 sms.py`. Do not type the token on the command line, or it
ends up in your shell history.

### PHP (curl extension, PHP 8.0+)

```php
<?php

declare(strict_types=1);

function tikker(string $base, string $token, string $method, string $path, ?array $body = null, array $headers = []): array
{
    $ch = curl_init($base . $path);
    $hdr = ['Authorization: Bearer ' . $token, 'Accept: application/json'];
    if ($body !== null) {
        $hdr[] = 'Content-Type: application/json';
        curl_setopt($ch, CURLOPT_POSTFIELDS, json_encode($body, JSON_THROW_ON_ERROR));
    }
    foreach ($headers as $name => $value) {
        $hdr[] = $name . ': ' . $value;
    }
    curl_setopt_array($ch, [
        CURLOPT_CUSTOMREQUEST => $method,
        CURLOPT_HTTPHEADER => $hdr,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT => 10,
    ]);
    $raw = curl_exec($ch);
    if ($raw === false) {
        $err = curl_error($ch);
        curl_close($ch);
        throw new RuntimeException('curl: ' . $err);
    }
    $code = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
    curl_close($ch);
    if ($code >= 400) {
        throw new RuntimeException("HTTP $code: $raw");
    }
    return json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
}

$base  = getenv('TIKKER_URL') ?: 'https://sms.example.com/api/v1';
$token = getenv('TIKKER_TOKEN') ?: throw new RuntimeException('TIKKER_TOKEN is not set');

// 1. Send; store the key together with the request so you can repeat it after a network error
$key  = bin2hex(random_bytes(16));
$sent = tikker($base, $token, 'POST', '/messages',
    ['to' => '+48600100200', 'body' => 'Twój kod weryfikacyjny: 482913', 'client_ref' => 'otp-7f3a91'],
    ['Idempotency-Key' => $key]);
$msgId = $sent['messages'][0]['id'];

// 2. Status
$msg = tikker($base, $token, 'GET', '/messages/' . $msgId);
echo $msg['status'], ' ', $msg['last_error'] ?? '-', PHP_EOL;

// 3. Incoming, all pages
$cursor = null;
do {
    $query = http_build_query(array_filter(
        ['direction' => 'in', 'limit' => 50, 'cursor' => $cursor],
        static fn ($v) => $v !== null,
    ));
    $page = tikker($base, $token, 'GET', '/messages?' . $query);
    foreach ($page['items'] as $m) {
        echo $m['created_at'], ' ', $m['peer'], ' ', $m['body'], PHP_EOL;
    }
    $cursor = $page['next_cursor'];
} while ($cursor !== null);
```

Run it with: set `TIKKER_TOKEN` as above (from a file, a secret store, or `read -rs`), then `php sms.php`.

## 12. Frequently asked questions

**The router is behind CGNAT (no public IP). What should I choose?**
You have two options, and neither needs port forwarding on the router.
1. **WireGuard**: a VPN tunnel links the router and the tikker server, and tikker reaches the router's REST API through
   the tunnel address. Setup: [deploy-server.md, section 6](deploy-server.md#6-routery-za-cgnat-wireguard) (in Polish).
2. **`agent` connector**: the router polls tikker over HTTPS, so only the tikker server needs a public address. No VPN
   and no public IP on the router are needed. See [agent.md](agent.md).

**Why do Polish letters make an SMS shorter?**
Polish letters are not part of GSM-7, so the SMS switches to UCS-2: 70 characters per SMS and 67 per segment of a long
message, instead of 160 and 153. If the text can do without the diacritics, turn on transliteration or write it without
them.

**Why do I get `422` `too_many_segments`?**
The text does not fit the effective segment limit (see section 4). Shorten the text, or set globally
`TIKKER_LONG_MESSAGE_STRATEGY=split` (several SMS messages, but the total is still capped by the channel's
`max_segments`) or `truncate` (cut the text). On the `routeros-rest`
connector the limit is one segment, because RouterOS does not concatenate SMS messages (assumption `ASSUMPTION(T00)`).

**Why do I get `422` `unsupported_characters`?**
The text contains characters outside GSM-7, and the channel supports neither UCS-2 nor transliteration. Turn on
`transliterate` on the channel, or remove those characters.

**What does status `sent` mean? Did the SMS arrive?**
`sent` means that the router accepted the SMS for sending. The modem does not return delivery reports, so tikker cannot
confirm that the SMS reached the phone.

**The router is offline. What happens to the messages?**
- After failed health checks, a channel changes to `degraded` or `down`. Such a channel takes no new messages: messages
  with no assigned channel are sent by another healthy channel, while messages pinned to a channel (`channel_id`) wait.
- A transient error (no network, a timeout) retries sending up to 5 times, with nominal delays of about 30 s, 2 min,
  8 min, and 30 min (±20% jitter). After the attempts are used up, the message ends as `failed`.
- A message with `expires_at` that was not sent before that time changes to `failed` with a `last_error` starting with
  `expired`.
- The guarantee is **at-least-once** ([ADR 0002](adr/0002-gwarancje-dostarczenia.md)). tikker does not lose messages,
  but in rare cases (an interrupted send, an expired lease) an SMS can go out twice. Then the `possible_duplicate`
  field is `true`.

**How many messages can I send?**
- API limit: by default 120 requests per minute per token (`TIKKER_API_RATE_LIMIT`). Exceeding it returns `429`.
- Channel limit: by default 6 SMS messages per minute (`rate_per_min`). This is not an error: excess messages wait in
  the queue, so with the default setting that is about one SMS every 10 s. You can raise the limit on a channel, up to
  600 per minute.

**How do I check that tikker is running?**
`GET /healthz` (alive) and `GET /readyz` (ready). In a container, `tikker healthcheck` returns exit code 0 when the
instance is ready; `tikker healthcheck --live` checks only liveness.

**Can I use the panel over plain HTTP?**
Only on a trusted local network and only for testing. On the internet, use TLS (Caddy in `deploy/docker`, or your own
reverse proxy). Passwords and session cookies sent over plain HTTP can be read on the network.

**I lost `secret.key`. What now?**
Without the original key, the stored router passwords cannot be read, and tikker will not start with this database if
it contains devices (the panel is unreachable). Restore `secret.key` or `TIKKER_SECRET_KEY` from a backup, or remove
the devices from the database and add them again. With a database that has no devices, the key is silently
regenerated. That is why you should back up `secret.key` together with the database.
