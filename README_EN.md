# CPA User Routing

[中文](./README.md) / English

`user-routing` is a plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI). **It lets different API keys use different pools of upstream accounts**, without clients having to change the model name.

## What problem does it solve?

Suppose you use model prefixes in CPA to separate account pools: `team-a/gpt-5` uses group A's accounts and `team-b/gpt-5` uses group B's. Normally each client would have to type the prefix itself to reach the right pool.

With this plugin installed:

- User A requests `gpt-5` with their key, and the plugin rewrites it to `team-a/gpt-5`.
- User B requests `gpt-5` with their key, and the plugin rewrites it to `team-b/gpt-5`.
- For any other key, or when the model does not exist under that prefix, the `default` prefix is used (it can be empty, which keeps the original model name).

It also offers a few optional features:

| Feature | In one sentence | Default |
| --- | --- | --- |
| [Hide prefixed models](#hide-prefixed-models) | The model list shows `gpt-5` instead of `team-a/gpt-5`, `team-b/gpt-5`, … | Off |
| [Switch groups when quota runs out](#switch-groups-when-quota-runs-out) | When group A's Codex quota is exhausted, use group B automatically; optionally pick whom to borrow from by live quota | Off |
| [Quota query and reset endpoints](#quota-query-and-reset-endpoints) | Check remaining account quota, or spend a reset credit, using an ordinary API key | On |

## Quick start

### 1. Install

Download the archive from [Releases](https://github.com/taoxian000/cpa-plugin-userrouting/releases) that matches CPA's platform and architecture, and put the library into CPA's plugin directory (the one `plugins.dir` points to):

| Platform | File name |
| --- | --- |
| Linux | `user-routing.so` |
| Windows | `user-routing.dll` |
| macOS | `user-routing.dylib` |

> The plugin ID comes from the file name. **Do not rename the file**, or the `configs.user-routing` section below will not apply.
>
> CPA itself must be built with `CGO_ENABLED=1`; otherwise it cannot load shared-library plugins.

### 2. Configure

Add this to CPA's `config.yaml`:

```yaml
# API keys used by clients. In CPA v8 these live under access.api-keys; older versions use top-level api-keys.
api-keys:
  - "key-of-user-a"
  - "key-of-user-b"

plugins:
  enabled: true
  dir: "plugins"
  configs:
    user-routing:
      enabled: true
      prefix_map:
        key-of-user-a: "team-a/"
        key-of-user-b: "team-b/"
        default: ""          # prefix used in all other cases; empty means no prefix
```

- The trailing `/` is optional: `team-a` and `team-a/` are the same.
- Every key in `prefix_map` must already be in CPA's API key list, or the plugin refuses to load the configuration (so a typo cannot silently disable a rule). To configure keys before adding them to CPA, set `strict_key_validation: false`.
- `prefix_map` can also be a one-line JSON string: `prefix_map: '{"key-of-user-a":"team-a/","default":""}'`.

See [config.example.yaml](./config.example.yaml) for a complete example.

### 3. Verify

Restart CPA and send a request. The CPA log should contain:

```text
msg="user-routing selected execution model" requested_model=gpt-5 model=team-a/gpt-5 used_default=false
```

- `requested_model`: the model the client asked for.
- `model`: the model actually used.
- `used_default`: whether `default` was used because the key has no mapping or the model does not exist under its prefix.
- After a quota switch, the line also includes `quota_fallback=true`.

The plugin never writes API keys to the log.

> CPA's raw HTTP request dump is recorded before the plugin runs, so it still shows the client's original model name. To see which model was actually used, rely on the line above, CPA's credential-selection log, and usage records.

## How it works

For each request, the plugin:

1. Reads the API key from the request header or query string and checks that it is in CPA's API key list. Unknown or missing keys always get `default`.
2. Looks up the key's prefix and queries CPA's own `/v1/models` to confirm that `prefix + model` (for example `team-a/gpt-5`) really exists.
3. Uses it if it exists; otherwise falls back to the `default` prefix.
4. Rewrites the top-level `model` field of the JSON body and hands the request back to CPA. Account selection and usage accounting then use the rewritten model.

The model list is cached separately for each key, for 5 seconds by default (`model_cache_ttl`). When CPA's config file changes, the plugin rereads its API key list automatically; the plugin's own settings are updated through CPA's plugin reload mechanism.

## Supported APIs

| API | Notes |
| --- | --- |
| OpenAI Chat Completions | Fully supported |
| OpenAI Responses | Fully supported |
| Claude Messages | Fully supported |
| Gemini | Fully supported |
| OpenAI Videos: `POST /v1/videos`, `/v1/videos/generations`, `/v1/videos/edits`, `/v1/videos/extensions`, `/openai/v1/videos` | Fully supported, including quota group switching |
| OpenAI Images: `POST /v1/images/generations`, `/v1/images/edits` (JSON and multipart) | Fully supported, including quota group switching; requires CPA v8.0.5+. Older CPA versions reject image requests forwarded by plugins |
| Codex Alpha Search: `POST /v1/alpha/search`, `/backend-api/codex/alpha/search` | Prefix routing supported, quota group switching **not** supported; requires CPA v7.2.95+. The body sent to Codex keeps the unprefixed model; the prefix is only used to let CPA pick the account |

Not handled by this plugin:

- **Claude `/v1/messages/count_tokens`**: CPA's plugin interface cannot yet let a plugin perform token counting through the host, so CPA's native behavior is kept.

## Optional features

### Hide prefixed models

With prefixes in use, clients see many entries like `team-a/gpt-5` and `team-b/gpt-5` in their model list. These settings tidy it up:

```yaml
register_deduplicated_models: true   # also register unprefixed, deduplicated names such as gpt-5 (requires CPA v7.2.155+)
hide_prefixed_models: true           # hide the prefixed entries from model lists (requires the option above)
include_default_prefix: true         # also strip a non-empty default prefix; on by default
```

Notes:

- A prefixed entry is hidden only when its unprefixed counterpart appears in the same list, so the list never ends up empty.
- This affects **only the list**. The models stay registered in CPA, and clients can still request `team-a/gpt-5` directly.
- Both regular and Claude-format model lists are handled (including `first_id` and `last_id` in Claude-format responses).
- `hide_prefixed_models` requires a CPA version that supports `response_interceptor` plugins.

### Switch groups when quota runs out

For Codex. When one group of accounts runs out of quota, the plugin retries with the next group's prefix, and the client does not notice.

```yaml
quota_fallback:
  enabled: true
  fallback_on_other_errors: false
  prefixes:
    team-a:          # when team-a runs out...
      - team-b       # ...try team-b first
      - team-c       # ...then team-c
```

**When does it switch?**

| Upstream response | Default | `fallback_on_other_errors: true` |
| --- | --- | --- |
| Codex `usage_limit_reached` (account quota exhausted) | Switch | Switch |
| HTTP 429 from CPA with error code `model_cooldown` (every account for the model is cooling down) | Switch | Switch |
| Plain HTTP 429 and other upstream errors | No switch | Switch |

Keep in mind:

- Only the listed prefixes are tried, in order, one level deep. It does not continue into `team-b`'s own fallback list.
- A streaming request never switches once content has been sent to the client.
- Switching prefixes does not clear CPA's account cooldown; CPA still picks accounts in the target group according to its own cooldown state.

#### Auto-sort

The `prefixes` map above is a fixed order per prefix. With auto-sort, the plugin periodically queries the Codex quota of a set of prefixes and uses the result to decide whom to borrow from when the nominal prefix is unavailable.

```yaml
quota_fallback:
  enabled: true                  # required for auto-sort to take effect
  auto_sort:
    enabled: true
    strategy: borrow             # borrow (default) or weekly_urgency
    five_hour_reserve: 25%       # 5-hour reserve; 25%, 25 or 0.25; default 25%
    refresh_interval: "5m"       # quota refresh interval; default 5 minutes, minimum 30 seconds
    prefixes: [team-a, team-b, team-c]
```

- **Scope**: only requests whose nominal prefix is listed in `auto_sort.prefixes` use auto-sort, and they only borrow from other prefixes in that list. Requests whose nominal prefix is not listed keep using their own list in `quota_fallback.prefixes`.
- **The nominal prefix is always tried first.** The sorted order is used only after it reports exhausted quota or model cooldown.
- **Immediate demotion**: when a prefix returns exhausted quota or model cooldown on a real request, it moves to the back right away instead of waiting for the next refresh.
- **Stale data**: if a cached window's reset time has passed, the window is treated as reset (full).
- **`/quota` agrees**: the `actual_prefix` returned by `GET /quota` uses the same order.
- Each refresh writes the resulting order to the CPA log at debug level (`user-routing auto-sort refreshed`). `auto_sort=true` in a request log line means that request used auto-sort.

**`borrow` strategy (default): borrow without hurting the account owner.** Meant for "everyone has their own accounts and uses them independently, but can borrow someone else's when they run short".

The plugin first estimates each account's **spare weekly quota**: projecting the owner's pace so far this week, how much will they still use before the weekly reset? Whatever is left over is quota they will not need.

```text
still needed = used this week ÷ time elapsed this week × time until weekly reset
spare weekly = weekly remaining − still needed
```

During the first day of a weekly window there is too little history, so the plugin conservatively assumes the owner will use all of the remaining quota. The weekly window is treated as 7 days.

Prefixes are then ordered as follows:

| Order | Group | Condition | Order within group |
| --- | --- | --- | --- |
| 1 | Safe to borrow | Some account has spare weekly > 0 and 5-hour remaining ≥ the reserve | Earliest 5-hour reset first (if it gets drained, the owner is affected for the shortest time); then more spare weekly first |
| 2 | Usable but affects the owner | Has a usable account but fails the condition above | More spare weekly first, then higher 5-hour remaining |
| 3 | No data | Not refreshed yet, or the query failed | Configured order |
| 4 | Exhausted | No usable account | Earliest recovery first |
| 5 | No accounts | No enabled Codex account under the prefix | Configured order |

**`weekly_urgency` strategy: spend weekly quota that is about to expire first.** Meant for treating all accounts as one shared pool. Usable prefixes are ordered by `weekly remaining ÷ hours until weekly reset`, highest first (summed over accounts in the prefix), followed by no-data, exhausted, and no-account prefixes. This strategy ignores `five_hour_reserve`.

### Quota query and reset endpoints

The plugin implements CPA's `QuotaProvider` (requires CPA v7.3.9+) and adds HTTP endpoints so that users with an ordinary API key can check quota without the CPA management key.

**Overview** (all paths start with `/v0/resource/plugins/user-routing`, omitted below):

| Endpoint | Purpose | Authentication | Side effects |
| --- | --- | --- | --- |
| `GET /quota` | Codex quota of the accounts behind your own key | CPA API key | No |
| `GET /quota?provider=...&email=...` | Quota of any specific account (Claude, Antigravity, etc.) | CPA API key + server-side `MANAGEMENT_PASSWORD` | No |
| `GET /quota?provider=kiro` | Quota of all Kiro accounts | CPA API key + server-side `MANAGEMENT_PASSWORD` | No |
| `GET /quota/direct` | Query a single Codex account with its login token | Codex access token | No |
| `GET /quota/reset` | Spend one reset credit on each Codex account behind your key | CPA API key | **Yes** |
| `GET /quota/direct/reset` | Spend one reset credit on a single account using its login token | Codex access token | **Yes** |
| `POST /v0/management/plugins/user-routing/quota/reset` | CPA management API: spend one reset credit on a given auth file | CPA management key | **Yes** |

> **The two reset GET endpoints really do spend reset credits.** Do not let browser prefetching, chat-app link previews, or auto-retrying clients hit them.

#### Your own Codex quota: `GET /quota`

```text
GET /v0/resource/plugins/user-routing/quota
Authorization: Bearer <CPA API key>
```

The plugin finds the key's prefix (the "nominal prefix") and checks whether that group's Codex accounts still have quota. If not, it checks the next groups in `quota_fallback.prefixes` order (or the [auto-sort](#auto-sort) order when enabled) and stops at the first group with quota (the "actual prefix").

An account counts as "having quota" only if every quota window (for example the 5-hour `primary` and weekly `secondary` windows) is above zero. If any window is at zero, the search continues.

Main response fields:

| Field | Meaning |
| --- | --- |
| `nominal_prefix` | The prefix this key is mapped to |
| `actual_prefix` | The prefix that would actually be used; equal to the nominal prefix while it still has quota |
| `nominal_accounts` | Accounts under the nominal prefix, keyed by email |
| `actual_accounts` | Accounts under the actual prefix; omitted when it equals the nominal prefix |

Each account has the standard quota fields plus `reset_credits` (remaining reset credits):

| Field | Meaning |
| --- | --- |
| `available_count` | Number of reset credits left |
| `expires_at` | Expiry times of the credits that expire |
| `without_expiry` | Number of credits that never expire |
| `expiry_details_available` | Whether expiry details were retrieved |
| `expiry_details_complete` | Whether the details cover every credit (the upstream may truncate them) |

Responses never include tokens, auth-file indexes, credit IDs, or raw auth JSON. Failed queries are retried up to 3 times.

#### A specific account: `GET /quota?provider=...&email=...`

```text
GET /v0/resource/plugins/user-routing/quota?provider=antigravity&email=account@example.com
Authorization: Bearer <CPA API key>
```

- Pass `provider` and `email` together; both are case-insensitive. Without them you get "your own Codex quota" above.
- Returns 409 if more than one auth file matches.
- Exception: `provider=kiro` without `email` returns **all** Kiro accounts.
- Claude and Antigravity are queried through their official quota APIs (the same way the CPA management panel does it), so this works even though CPA has no built-in quota support for them. Other providers go through CPA's native `quota/fetch` and need support from CPA or the relevant plugin.
- The response format matches Codex (`nominal_accounts`, `subscription`, `summary`, `groups[].buckets[]`) plus `partial: false`, but has no prefix fields and no `reset_credits`.
- Window names are unified: Claude's `five-hour`/`seven-day` and Antigravity's `5h`/`weekly` become `primary`/`secondary`, with the original name kept in `sourceWindow`. Both Antigravity groups, "Gemini Models" and "Claude and GPT models", are kept in full.

**All Kiro accounts** (`provider=kiro`): requires the [Kiro plugin](https://github.com/nguyenha935/kiro-cpa-plugin) to be installed and enabled. Each account gets one `Kiro` group with one `monthly` bucket. Accounts are keyed by email, falling back to label, file name, or `auth_index`; repeated emails get `auth_index` appended. If some accounts fail, the rest are returned with `partial: true`; if all fail, the response is 502; with no Kiro accounts, the dictionary is empty.

> **Permissions:** these lookups ignore the key's prefix, so **any** valid CPA API key can see these accounts' email addresses and quota. Only give API keys to people you trust.

**The server needs `MANAGEMENT_PASSWORD`.** These lookups go through CPA's management API, so `MANAGEMENT_PASSWORD` must be set in the CPA process environment. Otherwise they return 503 (the normal `/quota` query keeps working).

- You do not need to recover the original password behind the bcrypt hash in the config. Just generate a new strong random value; the existing config can stay as it is.
- Injecting it through a systemd `EnvironmentFile` with mode `0600` is recommended.
- The plugin never reads or cracks the config hash, and never puts this key in plugin config, client requests, responses, or logs.

> **Security warning:** in current CPA, setting `MANAGEMENT_PASSWORD` turns on CPA's **entire** management API and overrides `remote-management.allow-remote: false`. If CPA listens on a public or LAN address, others can try this key against the management API too. Restrict access with a firewall or listen on localhost only, and protect the environment file.
>
> The plugin itself only connects to CPA over loopback, without proxies or redirects, so the plugin never sends the key to another machine. In turn, CPA must listen on a loopback address or a wildcard address such as `0.0.0.0`; otherwise the lookup fails.

#### Query a Codex account directly: `GET /quota/direct`

```text
GET /v0/resource/plugins/user-routing/quota/direct
Authorization: Bearer <tokens.access_token from a Codex auth file>
ChatGPT-Account-ID: <tokens.account_id from a Codex auth file>
```

- No CPA API key, prefixes, or CPA auth files are involved. Anyone with a valid Codex access token can query it.
- Returns an `accounts` dictionary keyed by the email in the token (or the account ID if no email is found), with the same quota fields as above.
- `id_token`, `refresh_token`, and `base_url` are not needed; the default Codex endpoint is always used.
- Always queried live, never cached.
- Use HTTPS, never put the token in the URL, and make sure proxies and application logs do not record the `Authorization` header.

#### Spending reset credits

There are three ways:

| Method | Scope | Clears CPA's local cooldown |
| --- | --- | --- |
| `GET /quota/reset` (CPA API key) | Every enabled Codex account under the key's nominal prefix (including accounts CPA is cooling down), at most once each; does **not** follow `quota_fallback` | No |
| `GET /quota/direct/reset` (Codex token) | One account, once | No |
| `POST /v0/management/plugins/user-routing/quota/reset` (CPA management key) | One given auth file, once | **Yes** |

```text
GET /v0/resource/plugins/user-routing/quota/reset
Authorization: Bearer <CPA API key>
```

Example success:

```json
{"nominal_prefix":"team-a/","success":true,"partial":false,"nominal_accounts":{"account@example.com":{"success":true,"message":"Codex quota reset credit consumed"}}}
```

```text
GET /v0/resource/plugins/user-routing/quota/direct/reset
Authorization: Bearer <Codex tokens.access_token>
ChatGPT-Account-ID: <Codex tokens.account_id>   # optional; read from the token if omitted
```

Returns `success` and an `accounts` dictionary keyed by email (or account ID).

```text
POST /v0/management/plugins/user-routing/quota/reset
{"auth_index":"<CPA auth index>"}
```

This is CPA's native management API and needs the CPA management key. On success, CPA also clears that account's local cooldown.

For all three:

- The spend request is **sent once and never retried automatically** (the read-only checks before it may retry).
- If the connection times out before the result arrives, the reset may already have happened. Check the remaining count before trying again.
- The two GET endpoints call Codex directly, so CPA does not know a reset happened and does not clear its local cooldown. Use the management endpoint when you need that.

#### Quota cache

```yaml
quota_provider:
  enabled: true          # register the QuotaProvider
  public_endpoint: true  # expose the /v0/resource/... endpoints above; false keeps only CPA's native management API
  cache_ttl: "1m"        # how long successful quota queries are cached; 0s disables
```

- Successful results are cached per account (`provider + auth_index`); concurrent queries for the same account are merged into one. The cache entry is invalidated when the account's credentials change.
- Failed queries and incomplete reset-credit details are not cached.
- A cached entry expires early when one of its quota windows reaches its reset time.
- Before a Codex reset, the account's cache entry is cleared; `/quota/direct/reset` clears all Codex entries because it cannot tell which CPA account it is.
- `/quota/direct` never uses the cache.
- With caching on, quota numbers and the "actual prefix" choice can lag by up to one `cache_ttl`.
- If CPA marks a nominal-prefix account as unavailable but its `next_retry_after` has passed, `/quota` checks it again; unavailable accounts without a retry time are still skipped.

Codex quota data comes from ChatGPT's internal endpoints `/backend-api/wham/usage`, `/backend-api/wham/rate-limit-reset-credits`, and its `/consume` endpoint; see the [official Codex client implementation](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client/rate_limit_resets.rs). These are not public APIs and may change at any time.

## Configuration reference

| Field | Default | Description |
| --- | --- | --- |
| `enabled` | `true` | Enable the plugin |
| `prefix_map` | `{"default":""}` | Mapping from API keys to prefixes; `default` is the fallback |
| `strict_key_validation` | `true` | Refuse to load if `prefix_map` contains a key CPA does not know |
| `cpa_config_path` | Auto | Path to CPA's config file. Tries CPA's `-config` argument, then the `CPA_CONFIG_PATH` environment variable, then `./config.yaml` |
| `register_deduplicated_models` | `false` | Also register model names with the prefix removed |
| `hide_prefixed_models` | `false` | Hide prefixed entries from model lists; requires the option above |
| `include_default_prefix` | `true` | Also strip a non-empty `default` prefix |
| `quota_fallback.enabled` | `false` | Switch groups when quota runs out |
| `quota_fallback.fallback_on_other_errors` | `false` | Also switch on other upstream errors |
| `quota_fallback.prefixes` | Empty | For each prefix, the prefixes to try in order when it runs out |
| `quota_fallback.auto_sort.enabled` | `false` | Enable auto-sort |
| `quota_fallback.auto_sort.strategy` | `borrow` | Sort strategy: `borrow` (protect the owner) or `weekly_urgency` (spend expiring weekly quota first) |
| `quota_fallback.auto_sort.five_hour_reserve` | `25%` | 5-hour reserve for the `borrow` strategy |
| `quota_fallback.auto_sort.refresh_interval` | `5m` | Quota refresh interval; minimum `30s` |
| `quota_fallback.auto_sort.prefixes` | Empty | Prefixes that take part in auto-sort; at least 2 when enabled |
| `quota_provider.enabled` | `true` | Register the Codex quota provider |
| `quota_provider.public_endpoint` | `true` | Expose the `/v0/resource/plugins/user-routing/quota...` endpoints |
| `quota_provider.cache_ttl` | `1m` | Quota cache lifetime; `0s` disables |
| `models_url` | Auto | Full URL of CPA's `/v1/models`, normally derived from CPA's host, port, and TLS settings |
| `model_cache_ttl` | `5s` | Model list cache lifetime; `0s` disables |
| `model_lookup_timeout` | `3s` | Timeout for model list lookups |
| `models_tls_insecure_skip_verify` | `false` | Enable only if CPA's local HTTPS uses a self-signed certificate |
| `log_routing` | `true` | Log the final model in CPA's log |

About the API key list: older CPA versions use top-level `api-keys`. CPA v8 uses `access.api-keys`; v8's top-level `api-keys` holds upstream provider keys and is not treated as client keys.

## Known issues

- Claude token counting does not go through this plugin, and image endpoints require CPA v8.0.5+; see [Supported APIs](#supported-apis).

## Building from source

Requires Go 1.26+ and a C compiler for the target platform (the plugin uses cgo's `c-shared` mode).

```bash
# Linux
./scripts/build.sh
```

```powershell
# Windows (LLVM-MinGW recommended; some newer MinGW GCC versions emit PE BigObj files that Go cannot handle)
$env:CC = "x86_64-w64-mingw32-clang"
.\scripts\build.ps1

# Cross-compile Linux amd64 on Windows (requires Zig)
.\scripts\build.ps1 -Target linux
```

Output goes to `dist/`. Release builds inject the version with `-ldflags "-X main.pluginVersion=<version>"`.

### Releasing

Pushing a `v*` tag triggers GitHub Actions: tests run first, then builds for Windows, Linux, and macOS on `amd64` and `arm64`. Each platform is packaged as its own ZIP, with a `.sha256` file per ZIP and a combined `checksums.txt`.

```bash
git tag v0.2.3
git push main v0.2.3
```

## References

- [CLIProxyAPI plugin development documentation](https://help.router-for.me/plugin/development)
- [CLIProxyAPI model router documentation](https://help.router-for.me/plugin/model-router)
- [CLIProxyAPI official plugin store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)
- [CPA Key Policy](https://github.com/origin652/cpa-plugin-key-policy)
