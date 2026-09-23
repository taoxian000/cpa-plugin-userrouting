# CPA User Routing

[中文](./README.md)/English

`user-routing` is a CLIProxyAPI dynamic plugin. It adds different model prefixes based on the downstream API key that has been authenticated against CPA's native `api-keys` list.

## Behavior

Configuration:

```json
{"apikey_1":"prefix_1/","apikey_2":"prefix_2/","default":""}
```

When a request uses model `gpt-5`:

1. The plugin extracts the API key from the request header or query parameter and verifies that it is present in CPA's `api-keys` list.
2. If the key has a dedicated mapping, the plugin queries CPA's own `/v1/models` and checks whether, for example, `prefix_1/gpt-5` exists.
3. If it exists, the dedicated prefix is used; otherwise the `default` prefix is used. A prefix may be empty.
4. The top-level `model` in the JSON request body is rewritten, then the request is sent through CPA's native model-execution chain.
5. CPA's main log records `requested_model`, the final `model`, and `used_default`. Upstream credential selection and usage accounting also use the final model.

The plugin never writes API keys to its logs. The model catalog is cached for five seconds by default.

### Deduplicated model registration

Disabled by default. When `register_deduplicated_models` is enabled, the plugin uses CPA's model registration capability to add deduplicated, unprefixed model names; CPA's original prefixed models remain visible. `include_default_prefix` is enabled by default and includes a non-empty `default` prefix; disabling it limits stripping to API-key prefixes. This capability requires CPA v7.2.155 or later.

Known issue: this plugin currently cannot be enabled together with [cpa-plugin-codexcomp](https://github.com/uf-hy/cpa-plugin-codexcomp). Otherwise, Codex WebSocket requests may return 408 because the response stream does not receive `response.completed`.

### Codex cross-prefix quota fallback

This feature is disabled by default. When enabled, the plugin switches the rewritten model to configured successor prefixes only if CPA returns Codex's `usage_limit_reached` error (account quota exhausted). For example, after quota exhaustion for `prefix_1/gpt-5.5`, it can try `prefix_2/gpt-5.5` and then `prefix_3/gpt-5.5`.

```yaml
quota_fallback:
  enabled: true
  # Set to true to also fall back on other upstream errors.
  fallback_on_other_errors: false
  prefixes:
    prefix_1:
      - prefix_2
      - prefix_3
```

The list is applied for one level only and in the specified order. By default, only `usage_limit_reached` triggers a switch. Set `fallback_on_other_errors` to `true` to also switch on other upstream errors. A streaming request that has already emitted content never switches models. Every switch is recorded in the CPA main log with `quota_fallback=true`.

### Codex quota queries

The plugin implements CPA's `QuotaProvider` capability for Codex. This requires CPA v7.3.9 or later, which includes the `QuotaProvider` ABI. It also registers a public resource endpoint that does not require the CPA Management Key, but does require a valid downstream CPA API key:

```text
GET /v0/resource/plugins/user-routing/quota
Authorization: Bearer <CPA downstream API key>
```

The request key selects the nominal prefix from `prefix_map`. The plugin then queries Codex credentials for that prefix and the ordered successors in `quota_fallback.prefixes`. The response keeps only the entries matching the nominal and selected actual prefixes instead of returning the full `prefixes` list: `nominal_prefix` and `actual_prefix` identify the two prefixes, while `nominal_accounts` and `actual_accounts` are dictionaries keyed by credential email. Each account item includes the normalized quota fields and `reset_credits`: `available_count` is the number of available reset credits, `expires_at` lists expiry times for available credits with an expiry, `without_expiry` counts credits that do not expire, `expiry_details_available` indicates whether detailed expiry data was retrieved, and `expiry_details_complete` indicates whether the returned expiry details cover all available credits (the upstream may truncate the detail list). Credential indexes, tokens, credit IDs, and raw auth JSON are never returned.

The plugin also implements CPA's native `QuotaProvider.ResetQuota`, which can consume one Codex reset credit for a single auth file through CPA's native Management API, protected by the Management Key:

```text
POST /v0/management/plugins/user-routing/quota/reset
{"auth_index":"<CPA auth index>"}
```

After a successful reset, CPA also clears that auth file's local quota cooldown. This native reset endpoint requires the CPA Management Key.

The plugin also provides a synchronous GET reset endpoint authenticated by the downstream API key:

```text
GET /v0/resource/plugins/user-routing/quota/reset
Authorization: Bearer <CPA downstream API key>
```

It only processes enabled, available Codex auth files under the key's `nominal_prefix`; it does not follow `quota_fallback`. It makes at most one attempt to consume an available reset credit for each account, waits for all accounts to finish, and returns a `nominal_accounts` dictionary with per-account `success` and `message` values. It never retries automatically. This is a mutating GET request, so do not trigger it through browser prefetching, link previews, or automatic client retries. Because this public resource route calls Codex directly, it bypasses CPA's native Management API post-processing and does not clear CPA's local quota cooldown; use the native management endpoint above when local cooldown clearing is required. If the connection times out before the consumption result is returned, an account may already have been reset, but the plugin will not retry; query the remaining count first.

Example successful response:

```json
{"nominal_prefix":"prefix1/","success":true,"partial":false,"nominal_accounts":{"account@example.com":{"success":true,"message":"Codex quota reset credit consumed"}}}
```

```yaml
quota_provider:
  enabled: true
  public_endpoint: true
```

With `public_endpoint: false`, CPA's native `QuotaProvider` integration remains available, but the public query and reset resource routes are not registered. Codex usage and reset-credit details are read from ChatGPT/Codex's `/backend-api/wham/usage` and `/backend-api/wham/rate-limit-reset-credits` endpoints; consumption calls the corresponding `/consume` endpoint. These upstream formats may change. See the [Codex upstream client implementation](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client/rate_limit_resets.rs).

Supported APIs/protocols:

- OpenAI Chat Completions (`openai`)
- OpenAI Responses (`openai-response`)
- Claude Messages (`claude`)
- Gemini (`gemini`)
- OpenAI Videos: `POST /v1/videos`, `/v1/videos/generations`, `/v1/videos/edits`, `/v1/videos/extensions`, and `/openai/v1/videos` (`openai-video`)
- Codex Alpha Search: `POST /v1/alpha/search` and `/backend-api/codex/alpha/search` (`codex-alpha-search`, requires CLIProxyAPI v7.2.95 or later)

Video requests use the same API-key prefix map, model-catalog validation, logging, and quota-fallback behavior as message requests. Alpha Search resolves the prefix during model routing and asks CPA to select the Codex account with the final model; the request body sent to Codex keeps the unprefixed model. Alpha Search does not pass through the plugin executor, so `quota_fallback` is unavailable for this endpoint. Claude's `/v1/messages/count_tokens` does not pass through the plugin because the current CLIProxyAPI plugin ABI has no callback for performing token counting through the host. That endpoint retains CPA's native behavior.

Image endpoints still cannot be routed safely by this plugin. The plugin-executor callback for image endpoints does not carry an "allow image model" flag, so CPA rejects image-only models such as `gpt-image-*` and `grok-imagine-image*`. This capability requires a corresponding CPA core callback.

## Build

Go 1.26 or later and a C compiler for the target platform are required because CLIProxyAPI plugins use the `c-shared` ABI. On Windows, LLVM-MinGW's `x86_64-w64-mingw32-clang` is recommended; some newer MinGW GCC versions produce `PE BigObj` intermediate files that the current Go cgo parser does not support.

Windows:

```powershell
$env:CC = "x86_64-w64-mingw32-clang" # when LLVM-MinGW is on PATH
.\scripts\build.ps1
```

Linux:

```bash
./scripts/build.sh
```

To cross-compile a Linux `amd64` build on Windows, install Zig and run:

```powershell
.\scripts\build.ps1 -Target linux
```

Build artifacts are placed in `dist/`:

- Windows: `user-routing.dll`
- Linux: `user-routing.so`
- macOS: `user-routing.dylib`

Copy the dynamic library to CPA's `plugins.dir` directory. The plugin ID is determined by the filename and must remain `user-routing`.

### GitHub Release builds

Pushing a Git tag that matches `v*` automatically triggers GitHub Actions. The workflow runs tests first, then builds shared libraries for Windows, Linux, and macOS on `amd64` and `arm64`.

Each target is published as a separate ZIP asset containing only its corresponding `user-routing` shared library. The release also includes a `.sha256` file for every ZIP and an aggregated `checksums.txt`. For example, to publish a new version:

```bash
git tag v0.2.3
git push main v0.2.3
```

## Configuration

See [config.example.yaml](./config.example.yaml) for a complete example. The minimum configuration is:

```yaml
api-keys:
  - "apikey_1"
  - "apikey_2"

plugins:
  enabled: true
  dir: "plugins"
  configs:
    user-routing:
      enabled: true
      prefix_map:
        apikey_1: "prefix_1/"
        apikey_2: "prefix_2/"
        default: ""
```

You can also use the JSON string from the original requirement directly:

```yaml
prefix_map: '{"apikey_1":"prefix_1/","apikey_2":"prefix_2/","default":""}'
```

Non-empty prefixes are normalized to end with `/`, so `prefix_1` and `prefix_1/` are equivalent.

### Configuration fields

| Field | Default | Description |
| --- | --- | --- |
| `enabled` | `true` | Whether to enable the plugin. |
| `cpa_config_path` | Auto-discovered | CPA's `-config` argument is used first, then `CPA_CONFIG_PATH`, then `./config.yaml`. |
| `prefix_map` | `{"default":""}` | Mapping from native API keys to prefixes; `default` is the fallback prefix. |
| `register_deduplicated_models` | `false` | Register additional unprefixed, deduplicated models in CPA. |
| `include_default_prefix` | `true` | Include a non-empty `default` prefix in the deduplication prefix list. |
| `quota_fallback.enabled` | `false` | Enable cross-prefix model fallback when a Codex account returns `usage_limit_reached`. |
| `quota_fallback.fallback_on_other_errors` | `false` | Also perform cross-prefix fallback for other upstream errors; streaming requests only fall back before their first emitted payload. |
| `quota_fallback.prefixes` | Empty | Mapping from source prefixes to target prefixes to attempt in order. |
| `quota_provider.enabled` | `true` | Register the Codex `QuotaProvider`. |
| `quota_provider.public_endpoint` | `true` | Register the quota resource route without the Management Key while requiring a downstream API key. |
| `strict_key_validation` | `true` | Reject configuration when a mapped key does not exist in CPA's `api-keys`. |
| `models_url` | Auto-derived | Absolute URL of CPA's `/v1/models` endpoint. |
| `model_cache_ttl` | `5s` | Model catalog cache lifetime; `0s` disables caching. |
| `model_lookup_timeout` | `3s` | Timeout for local model-catalog lookups. |
| `models_tls_insecure_skip_verify` | `false` | Enable only when local CPA HTTPS uses an untrusted certificate. |
| `log_routing` | `true` | Record the final model in CPA's main log. |

When `strict_key_validation` is enabled, the plugin verifies that every API key in `prefix_map` exists in CPA's main `api-keys` list. It rejects a configuration load or reload if a key is missing, preventing a typo from silently disabling a routing rule. When disabled, keys may be configured before they are added to `api-keys`; however, a request uses a mapped prefix only when it carries a CPA-authenticated API key, and unmatched requests always use the `default` prefix.

If the CPA configuration file changes at runtime, the plugin rereads `api-keys` based on the file modification time. The plugin's own `prefix_map` is updated through CPA's plugin reconfiguration mechanism.

## Logging

By default, the plugin produces a log entry similar to:

```text
msg="user-routing selected execution model" requested_model=gpt-5 model=prefix_1/gpt-5 used_default=false
```

This entry is written through CPA's plugin logging callback and includes CPA's request ID. CPA then performs model execution, upstream credential selection, and usage accounting using `prefix_1/gpt-5`.

CPA's detailed raw HTTP request dump is captured by middleware before the plugin runs, so it retains the original client request body. Use this plugin's log, CPA's credential-selection log, and usage records as the source of truth for the final execution model.

## References

- [CLIProxyAPI plugin development documentation](https://help.router-for.me/plugin/development)
- [CLIProxyAPI model router documentation](https://help.router-for.me/plugin/model-router)
- [CLIProxyAPI official plugin store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)
- [CPA Key Policy](https://github.com/origin652/cpa-plugin-key-policy)
