# CPA User Routing

中文/[English](./README_EN.md)

`user-routing` 是一个 CLIProxyAPI 动态插件。它依据 CPA 原生 `api-keys` 中实际通过认证的下游 API Key，为请求模型添加不同前缀。

## 行为

配置：

```json
{"apikey_1":"prefix_1/","apikey_2":"prefix_2/","default":""}
```

收到模型 `gpt-5` 时：

1. 从请求头或查询参数中提取已经位于 CPA `api-keys` 列表内的 Key。
2. 若该 Key 有专属配置，查询 CPA 自己的 `/v1/models`，检查如 `prefix_1/gpt-5` 是否存在。
3. 存在时使用专属前缀；不存在时使用 `default` 前缀。前缀可以为空。
4. 改写 JSON 请求体中的顶层 `model`，再通过 CPA 原生模型执行链路发送请求。
5. CPA 主日志记录 `requested_model`、最终 `model` 和 `used_default`；上游凭证选择和用量记录也使用最终模型。

插件不会把 API Key 写入日志。模型目录默认缓存 5 秒。

### 去重模型注册

默认关闭。启用 `register_deduplicated_models` 后，插件会通过 CPA 的模型注册能力，额外注册去掉配置前缀并去重后的模型名。`include_default_prefix` 默认开启，非空的 `default` 前缀也会参与剥离；关闭后只使用各 API Key 对应的前缀。该能力需要 CPA v7.2.155 或更高版本。

可选的 `hide_prefixed_models: true` 会在模型目录响应中隐藏这些配置前缀对应的原始带前缀模型，仅显示去重后的无前缀模型；它依赖 `register_deduplicated_models: true`。插件会先完成模型发现，再启用目录过滤；如果没有发现任何可注册的去重模型，则保持原目录不变，避免把目录过滤空。这只是目录展示过滤，不会从 CPA 注册表删除模型，手动请求带前缀模型仍可正常路由。该过滤通过 CPA 的响应拦截器实现，需要支持 `response_interceptor` 插件能力的 CPA 版本。

已知问题：本插件目前不能与 [cpa-plugin-codexcomp](https://github.com/uf-hy/cpa-plugin-codexcomp) 同时启用，否则 Codex 的 WebSocket 请求可能因响应流未收到 `response.completed` 而返回 408。

### Codex 跨前缀额度回退

默认关闭。启用后，插件仅在 CPA 返回 Codex 的 `usage_limit_reached`（账号额度耗尽）时，把当前已改写模型按顺序替换为配置的后继前缀。例如，`prefix_1/gpt-5.5` 返回额度耗尽后，可改试 `prefix_2/gpt-5.5`，再试 `prefix_3/gpt-5.5`。

```yaml
quota_fallback:
  enabled: true
  # 设为 true 时，其他上游错误也会触发回退。
  fallback_on_other_errors: false
  prefixes:
    prefix_1:
      - prefix_2
      - prefix_3
```

该列表只执行一层、按给定顺序尝试。默认只有 `usage_limit_reached` 会触发切换；将 `fallback_on_other_errors` 设为 `true` 后，其他上游错误也会触发切换。已经向客户端输出内容的流式请求不会切换。每次切换都会由 CPA 主日志记录为 `quota_fallback=true`。

### Codex 额度查询

插件实现了 CPA 的 `QuotaProvider`，当前只支持 Codex。该能力需要包含 `QuotaProvider` ABI 的 CPA v7.3.9 或更高版本。启用后，CPA 自己的 Management API 可以按认证文件查询 Codex 的额度；同时默认注册一个不需要 CPA Management Key 的公开资源接口：

```text
GET /v0/resource/plugins/user-routing/quota
Authorization: Bearer <CPA 下游 API Key>
```

插件会读取请求 Key 对应的名义前缀，并按 `quota_fallback.prefixes` 依次查询后继前缀的 Codex 认证文件；找到首个仍有额度的前缀后即停止查询。响应只保留名义前缀和实际前缀对应的认证账户，不再返回完整的 `prefixes` 列表：`nominal_prefix` 与 `actual_prefix` 表示两种前缀，`nominal_accounts` 与 `actual_accounts` 是以认证账户邮箱为键的字典。如果名义前缀本身仍有额度，`actual_prefix` 仍返回名义前缀，但省略重复的 `actual_accounts`。每个账户项包含标准额度字段，以及 `reset_credits`：`available_count` 为剩余可用重置次数，`expires_at` 为已返回的可用重置次数对应的失效时间列表，`without_expiry` 为不设失效时间的次数，`expiry_details_available` 表示是否成功读取详细有效期，`expiry_details_complete` 表示上游返回的有效期明细是否覆盖全部可用次数（上游可能截断明细）。插件不会返回认证文件索引、令牌、credit ID 或原始认证 JSON。额度用量与重置额度的只读查询失败时会重试 3 次（最多 4 次尝试）；重置次数的实际消费请求不会重试。

也可直接用 Codex 标准认证信息查询单个账户，无需 CPA 下游 API Key，也不查前缀或 CPA 认证文件：

```text
GET /v0/resource/plugins/user-routing/quota/direct
Authorization: Bearer <Codex tokens.access_token>
ChatGPT-Account-ID: <Codex tokens.account_id>
```

响应使用 `accounts` 字典，键为 access token 中的账户邮箱（无法读取时使用 account ID；邮箱仅用于结果标识，凭据有效性由上游验证），值的额度字段与上面的账户项相同；此路由不返回任何前缀字段。`base_url`、`id_token` 和 `refresh_token` 不需要传入，查询使用默认 Codex 地址。此路由不验证 CPA 下游 Key，持有有效 Codex access token 的请求即可直接查询；请使用 HTTPS，不要把 token 放入 URL，并确保代理及应用日志不会记录 `Authorization`。用量和重置额度详情的只读查询失败时最多重试 3 次。

也可使用相同认证字段同步消耗该账户的一次可用额度重置次数：

```text
GET /v0/resource/plugins/user-routing/quota/direct/reset
Authorization: Bearer <Codex tokens.access_token>
ChatGPT-Account-ID: <Codex tokens.account_id>  # 可选；缺省时尝试从 access token 中读取
```

重置只需要 `access_token` 和 `account_id`；账户邮箱只用于响应中的账户标识，可从 access token 解码。`id_token`、`refresh_token`、`last_refresh` 和 `base_url` 均不需要，未提供 `base_url` 时使用默认 Codex 地址。响应包含 `success` 和以账户邮箱（或 account ID）为键的 `accounts` 结果。此接口不需要 CPA API Key，也不查询 CPA 认证文件或前缀。它是有副作用的 GET 请求：可用额度查询最多重试 3 次，但实际消费请求只发送一次；不要由浏览器预取、链接预览或自动重试客户端触发。消费由插件直接发送给 Codex，不会清除 CPA 本地额度冷却状态。请使用 HTTPS，并避免记录 `Authorization`。

插件也实现了 CPA 原生 `QuotaProvider.ResetQuota`，可从 CPA 受 Management Key 保护的原生管理接口对单个认证文件消耗一次 Codex 重置额度，例如：

```text
POST /v0/management/plugins/user-routing/quota/reset
{"auth_index":"<CPA auth index>"}
```

这会对指定认证文件执行一次消费；成功后 CPA 会同步清除该认证文件的本地额度冷却状态。此原生重置接口需要 CPA Management Key。

插件另提供一个由下游 API Key 鉴权的同步 GET 重置接口：

```text
GET /v0/resource/plugins/user-routing/quota/reset
Authorization: Bearer <CPA 下游 API Key>
```

该接口只处理此 Key 对应的 `nominal_prefix` 下启用且可用的 Codex 认证文件，不跟随 `quota_fallback`；对每个账户最多尝试消费一次可用重置额度，等待所有账户处理完后返回 `nominal_accounts` 字典及逐账户 `success`、`message`。实际消费请求不会自动重试；前置的只读额度查询仍按上文规则最多重试 3 次。注意这是有副作用的 GET 请求，请勿由浏览器预取、链接预览或自动重试客户端触发。该公开资源路由直接调用 Codex 上游，成功后不会经过 CPA 原生 Management API 的后处理，因此不会清除 CPA 本地额度冷却状态；需要清除本地冷却时仍须使用上面的 CPA 原生管理接口。若连接在消费结果返回前超时，账户可能已被重置，但插件不会重试；请先重新查询剩余次数。

成功时的响应示例：

```json
{"nominal_prefix":"prefix1/","success":true,"partial":false,"nominal_accounts":{"account@example.com":{"success":true,"message":"Codex quota reset credit consumed"}}}
```

```yaml
quota_provider:
  enabled: true
  public_endpoint: true
```

`public_endpoint: false` 时仍保留 CPA 原生 `QuotaProvider` 能力，但不注册公开查询和重置资源接口。Codex 用量和重置额度详情分别通过 ChatGPT/Codex 的 `/backend-api/wham/usage` 与 `/backend-api/wham/rate-limit-reset-credits` 读取，消费则调用对应的 `/consume` 接口；这些上游接口格式可能变化。[Codex 上游客户端实现](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client/rate_limit_resets.rs)

支持的 API/协议：

- OpenAI Chat Completions (`openai`)
- OpenAI Responses (`openai-response`)
- Claude Messages (`claude`)
- Gemini (`gemini`)
- OpenAI Videos：`POST /v1/videos`、`/v1/videos/generations`、`/v1/videos/edits`、`/v1/videos/extensions`，以及 `/openai/v1/videos`（`openai-video`）
- Codex Alpha Search：`POST /v1/alpha/search` 和 `/backend-api/codex/alpha/search`（`codex-alpha-search`，需要 CLIProxyAPI v7.2.95 或更高版本）

视频请求与消息请求使用同一套 API Key 前缀、模型目录校验、日志与额度回退逻辑。Alpha Search 在模型路由阶段解析前缀，并让 CPA 使用最终模型选择 Codex 账户；发送至 Codex 的请求体仍保留无前缀模型。Alpha Search 不经过插件执行器，因此不支持 `quota_fallback`。Claude `/v1/messages/count_tokens` 不经过插件，因为当前 CLIProxyAPI 插件 ABI 没有“通过主程序执行 token count”的回调；该接口保留 CPA 原生行为。

图像端点当前仍不能由本插件安全路由：图像端点的插件执行器回调没有传递“允许图像模型”标记，因此 CPA 会拒绝 `gpt-image-*`、`grok-imagine-image*` 等图像专用模型。该功能需要 CPA 主程序增加对应的插件回调。

## 构建

需要 Go 1.26 或更高版本以及目标平台的 C 编译器，因为 CLIProxyAPI 插件使用 `c-shared` ABI。Windows 建议使用 LLVM-MinGW 的 `x86_64-w64-mingw32-clang`；部分较新的 MinGW GCC 会生成当前 Go cgo 解析器不支持的 `PE BigObj` 中间文件。

Windows：

```powershell
$env:CC = "x86_64-w64-mingw32-clang" # LLVM-MinGW 已在 PATH 时
.\scripts\build.ps1
```

Linux：

```bash
./scripts/build.sh
```

在 Windows 上交叉编译 Linux `amd64` 版本时，安装 Zig 后运行：

```powershell
.\scripts\build.ps1 -Target linux
```

生成文件位于 `dist/`：

- Windows: `user-routing.dll`
- Linux: `user-routing.so`
- macOS: `user-routing.dylib`

将动态库复制到 CPA 的 `plugins.dir` 目录。插件 ID 由文件名决定，必须保持为 `user-routing`。

### GitHub Release 构建

推送匹配 `v*` 的 Git 标签会自动触发 GitHub Actions。工作流先运行测试，然后构建 Windows、Linux、macOS 的 `amd64` 与 `arm64` 动态库。

每个目标平台会作为独立 ZIP 资产发布，ZIP 内只包含对应的 `user-routing` 动态库；Release 还包含每个 ZIP 的 `.sha256` 文件和汇总的 `checksums.txt`。例如，发布新版本：

```bash
git tag v0.2.3
git push main v0.2.3
```

## 配置

完整示例见 [config.example.yaml](./config.example.yaml)。最小配置如下：

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

也可直接使用题目给出的 JSON 字符串：

```yaml
prefix_map: '{"apikey_1":"prefix_1/","apikey_2":"prefix_2/","default":""}'
```

非空前缀会规范化为以 `/` 结尾，因此 `prefix_1` 与 `prefix_1/` 等价。

### 配置项

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 是否启用插件 |
| `cpa_config_path` | 自动发现 | 优先使用 CPA 的 `-config` 参数，其次 `CPA_CONFIG_PATH`，最后 `./config.yaml` |
| `prefix_map` | `{"default":""}` | 原生 Key 到前缀的映射；`default` 为回退前缀 |
| `register_deduplicated_models` | `false` | 是否向 CPA 额外注册去前缀并去重的模型 |
| `hide_prefixed_models` | `false` | 是否在模型目录响应中隐藏配置前缀模型；要求开启 `register_deduplicated_models`，只影响展示、不禁用路由 |
| `include_default_prefix` | `true` | 是否将非空 `default` 前缀加入去重前缀列表 |
| `quota_fallback.enabled` | `false` | 是否在 Codex 账号返回 `usage_limit_reached` 时启用跨前缀模型回退 |
| `quota_fallback.fallback_on_other_errors` | `false` | 是否也在其他上游错误时进行跨前缀模型回退；流式请求仅在输出首个内容前回退 |
| `quota_fallback.prefixes` | 空 | 源前缀到按顺序尝试的目标前缀列表 |
| `quota_provider.enabled` | `true` | 是否注册 Codex `QuotaProvider` |
| `quota_provider.public_endpoint` | `true` | 是否注册公开额度查询/重置资源接口：按前缀查询的接口要求下游 API Key，`/quota/direct` 则直接接受 Codex access token |
| `strict_key_validation` | `true` | 映射中出现不在 CPA `api-keys` 内的 Key 时拒绝加载 |
| `models_url` | 自动推导 | CPA `/v1/models` 的绝对地址 |
| `model_cache_ttl` | `5s` | 模型目录缓存时间，`0s` 表示不缓存 |
| `model_lookup_timeout` | `3s` | 本机模型目录查询超时 |
| `models_tls_insecure_skip_verify` | `false` | 仅在 CPA 本机 HTTPS 使用不受信证书时开启 |
| `log_routing` | `true` | 在 CPA 主日志中记录最终模型 |

`strict_key_validation` 开启时，插件会验证 `prefix_map` 中的每个 API Key 都存在于 CPA 主配置的 `api-keys` 列表；发现不存在的 Key 时会拒绝加载或重载配置，以避免拼写错误造成规则静默失效。关闭后可提前配置尚未加入 `api-keys` 的 Key，但实际请求仍只有携带 CPA 已认证 API Key 时才会使用对应前缀，未匹配时始终使用 `default` 前缀。

如果 CPA 配置文件在运行时更新，插件会按文件修改时间重新读取 `api-keys`。插件自身的 `prefix_map` 由 CPA 的插件重配置机制更新。

## 日志说明

默认会生成类似日志：

```text
msg="user-routing selected execution model" requested_model=gpt-5 model=prefix_1/gpt-5 used_default=false
```

这条日志由 CPA 主程序的插件日志回调写入，并带 CPA 请求 ID。随后 CPA 的模型执行、上游凭证选择和用量统计都以 `prefix_1/gpt-5` 执行。

CPA 的详细原始 HTTP request dump 是在插件运行前由中间件捕获，因此其中会保留客户端原始请求体；最终执行模型以本插件日志、CPA 凭证选择日志及用量记录为准。

## 相关资料

- [CLIProxyAPI 插件开发文档](https://help.router-for.me/plugin/development)
- [CLIProxyAPI 模型路由器文档](https://help.router-for.me/plugin/model-router)
- [CLIProxyAPI 官方插件商店](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)
- [CPA Key Policy](https://github.com/origin652/cpa-plugin-key-policy)
