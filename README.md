# CPA User Routing

中文 / [English](./README_EN.md)

`user-routing` 是 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI) 的一个插件。**它能让不同的 API Key 使用不同的上游账号池**，而客户端完全不用改模型名。

## 它解决什么问题？

假设你在 CPA 里给模型加了前缀来区分账号池：`team-a/gpt-5` 走 A 组账号，`team-b/gpt-5` 走 B 组账号。通常客户端得自己写上前缀才能选到对应的账号池。

装上这个插件后：

- 用户 A 用自己的 Key 请求 `gpt-5`，插件自动改成 `team-a/gpt-5`；
- 用户 B 用自己的 Key 请求 `gpt-5`，插件自动改成 `team-b/gpt-5`；
- 其他 Key，或对应前缀下根本没有这个模型时，使用 `default` 前缀（可以为空，即保持原模型名）。

另外，它还提供几个可选功能：

| 功能 | 一句话说明 | 默认 |
| --- | --- | --- |
| [隐藏带前缀的模型](#隐藏带前缀的模型) | 模型列表里只显示 `gpt-5`，不显示 `team-a/gpt-5`、`team-b/gpt-5` | 关闭 |
| [额度用完自动换组](#额度用完自动换组) | A 组 Codex 额度耗尽时，自动改用 B 组；可按实时额度自动决定借谁的 | 关闭 |
| [额度查询与重置接口](#额度查询与重置接口) | 用普通 API Key 就能查看账号剩余额度，或消耗一次重置次数 | 开启 |

## 快速开始

### 1. 安装

从 [Releases](https://github.com/taoxian000/cpa-plugin-userrouting/releases) 下载与 CPA 相同平台和架构的压缩包，解压后把动态库放进 CPA 的插件目录（即配置里 `plugins.dir` 指向的目录）：

| 平台 | 文件名 |
| --- | --- |
| Linux | `user-routing.so` |
| Windows | `user-routing.dll` |
| macOS | `user-routing.dylib` |

> 插件 ID 来自文件名，**不要重命名**，否则下面的 `configs.user-routing` 配置不会生效。
>
> CPA 主程序必须是以 `CGO_ENABLED=1` 编译的版本，否则无法加载动态库插件。

### 2. 配置

在 CPA 的 `config.yaml` 中加入：

```yaml
# 客户端使用的 API Key。CPA v8 中位于 access.api-keys，旧版位于顶层 api-keys。
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
        default: ""          # 其他情况使用的前缀；空字符串表示不加前缀
```

- 前缀末尾的 `/` 可写可不写，`team-a` 与 `team-a/` 等价。
- `prefix_map` 里的 Key 必须已在 CPA 的 API Key 列表中，否则插件会拒绝加载配置（防止拼写错误导致规则悄悄失效）。如需提前配置还没加入 CPA 的 Key，可设置 `strict_key_validation: false`。
- `prefix_map` 也可以写成一行 JSON：`prefix_map: '{"key-of-user-a":"team-a/","default":""}'`。

完整示例见 [config.example.yaml](./config.example.yaml)。

### 3. 确认生效

重启 CPA 并发一个请求，CPA 日志中应出现：

```text
msg="user-routing selected execution model" requested_model=gpt-5 model=team-a/gpt-5 used_default=false
```

- `requested_model`：客户端请求的模型；
- `model`：实际使用的模型；
- `used_default`：是否因 Key 未配置或前缀下没有该模型而使用了 `default`；
- 发生过额度切换时，还会带上 `quota_fallback=true`。

插件绝不会在日志中写入 API Key 明文。

> CPA 的原始 HTTP 请求转储是在插件运行之前记录的，里面看到的仍是客户端发来的原始模型名。实际使用哪个模型，以上面这条日志以及 CPA 的凭证选择日志、用量记录为准。

## 工作原理

收到请求时，插件依次执行：

1. 从请求头或查询参数中取出 API Key，确认它在 CPA 的 API Key 列表中。未知或缺失的 Key 一律使用 `default`。
2. 查出这个 Key 对应的前缀，再查询 CPA 自己的 `/v1/models`，确认 `前缀 + 模型名`（如 `team-a/gpt-5`）确实存在。
3. 存在就用它；不存在就改用 `default` 前缀。
4. 改写请求 JSON 顶层的 `model` 字段，然后交给 CPA 正常处理。之后的账号选择和用量统计都按改写后的模型进行。

模型列表按 Key 分别缓存，默认 5 秒（`model_cache_ttl`）。CPA 配置文件被修改后，插件会自动重新读取其中的 API Key 列表；插件自身的配置随 CPA 的插件重载机制更新。

## 支持的接口

| 接口 | 说明 |
| --- | --- |
| OpenAI Chat Completions | 完整支持 |
| OpenAI Responses | 完整支持 |
| Claude Messages | 完整支持 |
| Gemini | 完整支持 |
| OpenAI Videos：`POST /v1/videos`、`/v1/videos/generations`、`/v1/videos/edits`、`/v1/videos/extensions`、`/openai/v1/videos` | 完整支持，包括额度自动换组 |
| OpenAI Images：`POST /v1/images/generations`、`/v1/images/edits`（JSON 与 multipart） | 完整支持，包括额度自动换组；需要 CPA v8.0.5+，更早版本的 CPA 会拒绝插件转发的图像请求 |
| Codex Alpha Search：`POST /v1/alpha/search`、`/backend-api/codex/alpha/search` | 支持前缀路由，**不支持**额度自动换组；需要 CPA v7.2.95+。发给 Codex 的请求体仍是无前缀模型名，前缀只用于让 CPA 选择账号 |

不经过本插件的接口：

- **Claude `/v1/messages/count_tokens`**：CPA 插件接口目前无法让插件代为执行 token 计数，因此保持 CPA 原生行为。

## 可选功能

### 隐藏带前缀的模型

开启前缀后，客户端的模型列表里会同时出现 `team-a/gpt-5`、`team-b/gpt-5` 等一堆条目。下面的配置可以让列表更干净：

```yaml
register_deduplicated_models: true   # 额外注册去掉前缀、去重后的模型名，如 gpt-5（需要 CPA v7.2.155+）
hide_prefixed_models: true           # 在模型列表中隐藏带前缀的原条目（需要上一项开启）
include_default_prefix: true         # 非空的 default 前缀也参与去前缀，默认开启
```

说明：

- 只有在同一份列表里存在对应的无前缀条目时，带前缀的条目才会被隐藏，所以列表不会被过滤成空的。
- 这只影响**列表展示**。模型仍在 CPA 中注册，客户端直接请求 `team-a/gpt-5` 依然可以正常使用。
- 普通格式和 Claude 格式的模型列表都会处理（包括 Claude 格式响应中的 `first_id`、`last_id`）。
- `hide_prefixed_models` 需要 CPA 支持 `response_interceptor` 插件能力。

### 额度用完自动换组

适用于 Codex。当某组账号的额度用完时，插件自动把模型换成下一组的前缀重试，客户端感知不到。

```yaml
quota_fallback:
  enabled: true
  fallback_on_other_errors: false
  prefixes:
    team-a:          # team-a 额度用完时……
      - team-b       # ……先试 team-b
      - team-c       # ……再试 team-c
```

**什么时候会切换？**

| 上游返回 | 默认 | `fallback_on_other_errors: true` |
| --- | --- | --- |
| Codex `usage_limit_reached`（账号额度耗尽） | 切换 | 切换 |
| CPA 返回 HTTP 429 且错误码为 `model_cooldown`（该模型的所有账号都在冷却） | 切换 | 切换 |
| 普通 HTTP 429 及其他上游错误 | 不切换 | 切换 |

注意：

- 只按列表顺序尝试一层，不会从 `team-b` 再跳到 `team-b` 自己配置的后继。
- 流式请求只要已经向客户端输出了内容，就不会再切换。
- 切换前缀不会解除 CPA 对账号的冷却，目标组的账号依旧由 CPA 根据自己的冷却状态挑选。

#### 自动排序

上面的 `prefixes` 是每个前缀各自写死的顺序。开启自动排序后，插件会定期查询一组前缀的 Codex 额度，并根据结果决定"名义前缀不可用时先借谁的"。

```yaml
quota_fallback:
  enabled: true                  # 必须开启，自动排序才生效
  auto_sort:
    enabled: true
    strategy: borrow             # borrow（默认）或 weekly_urgency
    five_hour_reserve: 25%       # 5 小时保留线，可写 25%、25 或 0.25，默认 25%
    refresh_interval: "5m"       # 额度刷新间隔，默认 5 分钟，最短 30 秒
    prefixes: [team-a, team-b, team-c]
```

- **适用范围**：只有名义前缀在 `auto_sort.prefixes` 里的请求使用自动排序，并且只在这个列表内部互相借用。名义前缀不在列表里的请求，继续使用 `quota_fallback.prefixes` 中它自己的列表。
- **名义前缀总是先试**。只有名义前缀返回额度耗尽或模型冷却时，才按排序结果往下找。
- **立即降级**：某个前缀在真实请求中返回额度耗尽或模型冷却后，插件会立刻把它排到后面，不等下次刷新。
- **数据过期处理**：缓存中某个窗口的重置时间已经过去时，按已重置（额度满）计算。
- **`/quota` 一致**：`GET /quota` 返回的 `actual_prefix` 也按同一个排序结果计算。
- 每次刷新的排序结果会以 debug 级别写入 CPA 日志（`user-routing auto-sort refreshed`）；请求日志中的 `auto_sort=true` 表示本次请求使用了自动排序。

**`borrow` 策略（默认）：借用时尽量不影响账号主人。** 适合"每人有自己的账号，平时各用各的，额度突然不够时借别人的顶一下"。

插件先估算每个账号的**每周闲置额度**：按主人本周已用的速度，推算到每周重置前他还要用多少，剩下的就是他用不上的。

```text
还要用 = 本周已用 ÷ 本周已过时间 × 距离每周重置的时间
每周闲置 = 每周剩余 − 还要用
```

每周窗口开始不到 1 天时数据太少，按"主人会把剩余额度全部用完"保守估算。每周窗口按 7 天计算。

然后按以下顺序排列各前缀：

| 顺序 | 分组 | 条件 | 组内排序 |
| --- | --- | --- | --- |
| 1 | 可以放心借 | 有账号的每周闲置 > 0，且 5 小时剩余 ≥ 保留线 | 5 小时窗口最早重置的优先（万一借空，主人受影响的时间最短）；再按每周闲置从多到少 |
| 2 | 能借但会影响主人 | 有可用账号，但不满足上一条 | 每周闲置从多到少，再按 5 小时剩余从高到低 |
| 3 | 没有数据 | 还没刷新到，或查询失败 | 配置顺序 |
| 4 | 已用完 | 所有账号都不可用 | 恢复时间从早到晚 |
| 5 | 没有账号 | 该前缀下没有启用的 Codex 账号 | 配置顺序 |

**`weekly_urgency` 策略：优先用快要作废的每周额度。** 适合把所有账号当成一个共享池。可用前缀按 `每周剩余 ÷ 距离每周重置的小时数` 从高到低排序（同一前缀多个账号时求和），然后是没有数据、已用完、没有账号的前缀。这个策略不使用 `five_hour_reserve`。

### 额度查询与重置接口

插件实现了 CPA 的 `QuotaProvider`（需要 CPA v7.3.9+），并额外提供几个 HTTP 接口，让持有普通 API Key 的用户也能查看额度，不需要 CPA 管理密钥。

**接口一览**（路径前缀均为 `/v0/resource/plugins/user-routing`，以下省略）：

| 接口 | 用途 | 认证方式 | 有副作用 |
| --- | --- | --- | --- |
| `GET /quota` | 查看自己这个 Key 对应的 Codex 账号额度 | CPA API Key | 否 |
| `GET /quota?provider=...&email=...` | 查看任意指定账号的额度（Claude、Antigravity 等） | CPA API Key + 服务端 `MANAGEMENT_PASSWORD` | 否 |
| `GET /quota?provider=kiro` | 查看所有 Kiro 账号的额度 | CPA API Key + 服务端 `MANAGEMENT_PASSWORD` | 否 |
| `GET /quota/direct` | 用 Codex 登录令牌直接查单个账号 | Codex access token | 否 |
| `GET /quota/reset` | 给自己这个 Key 对应的所有 Codex 账号各消耗一次重置次数 | CPA API Key | **是** |
| `GET /quota/direct/reset` | 用 Codex 登录令牌给单个账号消耗一次重置次数 | Codex access token | **是** |
| `POST /v0/management/plugins/user-routing/quota/reset` | CPA 管理接口：给指定认证文件消耗一次重置次数 | CPA 管理密钥 | **是** |

> **两个重置用的 GET 接口会真的消耗重置次数。** 不要让浏览器预加载、聊天软件链接预览或会自动重试的客户端访问它们。

#### 查看自己的 Codex 额度：`GET /quota`

```text
GET /v0/resource/plugins/user-routing/quota
Authorization: Bearer <CPA API Key>
```

插件根据这个 Key 找到它的前缀（称为“名义前缀”），检查该组 Codex 账号是否还有额度。如果没有，就按 `quota_fallback.prefixes` 的顺序（开启[自动排序](#自动排序)时按自动排序的顺序）检查后继组，找到第一个有额度的组为止（称为“实际前缀”）。

一个账号的所有额度窗口（如 5 小时窗口 `primary`、每周窗口 `secondary`）都大于 0，才算“有额度”；任一窗口为 0 就继续往下找。

主要返回字段：

| 字段 | 含义 |
| --- | --- |
| `nominal_prefix` | 这个 Key 本来对应的前缀 |
| `actual_prefix` | 实际会用到的前缀；名义前缀还有额度时两者相同 |
| `nominal_accounts` | 名义前缀下的账号，以邮箱为键 |
| `actual_accounts` | 实际前缀下的账号；与名义前缀相同时省略 |

每个账号包含标准额度字段，以及 `reset_credits`（剩余重置次数）：

| 字段 | 含义 |
| --- | --- |
| `available_count` | 剩余可用的重置次数 |
| `expires_at` | 有过期时间的那些次数各自的过期时间 |
| `without_expiry` | 不会过期的次数 |
| `expiry_details_available` | 是否成功读取到过期时间明细 |
| `expiry_details_complete` | 明细是否覆盖了全部次数（上游可能会截断） |

响应中不会包含令牌、认证文件索引、credit ID 或原始认证 JSON。查询失败会自动重试最多 3 次。

#### 查看指定账号：`GET /quota?provider=...&email=...`

```text
GET /v0/resource/plugins/user-routing/quota?provider=antigravity&email=account@example.com
Authorization: Bearer <CPA API Key>
```

- `provider` 和 `email` 要一起提供，不区分大小写。都不提供时，就是上面的“查看自己的 Codex 额度”。
- 匹配到多个认证文件时返回 409。
- 例外：`provider=kiro` 且不带 `email` 时，返回**所有** Kiro 账号。
- Claude 和 Antigravity 会直接调用官方额度接口（与 CPA 管理面板相同的方式），即使 CPA 本身不支持这两家的额度查询也能用；其他 Provider 走 CPA 原生的 `quota/fetch`，需要 CPA 或对应插件支持。
- 返回格式与 Codex 一致（`nominal_accounts`、`subscription`、`summary`、`groups[].buckets[]`），外加 `partial: false`，但没有前缀字段和 `reset_credits`。
- 时间窗口统一命名：Claude 的 `five-hour`/`seven-day` 与 Antigravity 的 `5h`/`weekly` 分别映射为 `primary`/`secondary`，原名保留在 `sourceWindow`。Antigravity 的“Gemini Models”和“Claude and GPT models”两个分组都会完整保留。

**Kiro 全部账号**（`provider=kiro`）：需要已安装并启用 [Kiro 插件](https://github.com/nguyenha935/kiro-cpa-plugin)。每个账号返回一个 `Kiro` 分组、一个 `monthly` 额度条。账号以邮箱为键，没有邮箱时用标签、文件名或 `auth_index`，邮箱重复时在后面附加 `auth_index`。部分账号失败时返回可用部分并标记 `partial: true`；全部失败返回 502；没有 Kiro 账号时返回空字典。

> **权限提醒：** 这类查询不受 Key 的前缀限制，**任何**有效的 CPA API Key 都能看到这些账号的邮箱和额度。只把 API Key 发给可信的人。

**服务端需要设置 `MANAGEMENT_PASSWORD`。** 这类查询要借助 CPA 管理接口，所以 CPA 进程的环境变量中必须有 `MANAGEMENT_PASSWORD`，否则返回 503（不影响普通的 `/quota` 查询）。

- 不需要找回配置文件里 bcrypt 哈希对应的原密码，直接生成一个新的强随机值即可，原配置可以保留。
- 推荐通过权限为 `0600` 的 systemd `EnvironmentFile` 注入。
- 插件不会读取或破解配置中的哈希，也不会把这个密钥放进插件配置、客户端请求、响应或日志。

> **安全警告：** 按 CPA 当前的实现，设置 `MANAGEMENT_PASSWORD` 会开启 CPA **整套**管理接口，并且会绕过 `remote-management.allow-remote: false`。如果 CPA 监听在公网或局域网地址上，别人也可以拿这个密钥尝试访问管理接口。请用防火墙限制访问或只监听本机，并保护好环境变量文件。
>
> 插件自己只会通过本机回环地址连接 CPA，不走代理、不跟随重定向，所以插件不会把密钥发到别的机器。相应地，CPA 必须监听回环地址或 `0.0.0.0` 这类通配地址，否则查询会直接失败。

#### 不经过 CPA，直接查 Codex 账号：`GET /quota/direct`

```text
GET /v0/resource/plugins/user-routing/quota/direct
Authorization: Bearer <Codex 认证文件中的 tokens.access_token>
ChatGPT-Account-ID: <Codex 认证文件中的 tokens.account_id>
```

- 不需要 CPA API Key，也不涉及前缀和 CPA 认证文件。只要有有效的 Codex access token 就能查询。
- 返回 `accounts` 字典，键是 token 中的邮箱（读不到时用 account ID），额度字段同上。
- 不需要 `id_token`、`refresh_token`、`base_url`，始终使用默认的 Codex 地址。
- 每次都是实时查询，不使用缓存。
- 请使用 HTTPS，不要把 token 放在 URL 里，并确保代理和应用日志不会记录 `Authorization` 头。

#### 消耗重置次数

有三种方式，区别如下：

| 方式 | 作用范围 | 是否清除 CPA 本地冷却 |
| --- | --- | --- |
| `GET /quota/reset`（CPA API Key） | 这个 Key 名义前缀下所有未禁用的 Codex 账号（包括 CPA 正在冷却的账号），每个最多一次；**不**跟随 `quota_fallback` | 否 |
| `GET /quota/direct/reset`（Codex token） | 单个账号一次 | 否 |
| `POST /v0/management/plugins/user-routing/quota/reset`（CPA 管理密钥） | 指定的一个认证文件一次 | **是** |

```text
GET /v0/resource/plugins/user-routing/quota/reset
Authorization: Bearer <CPA API Key>
```

成功示例：

```json
{"nominal_prefix":"team-a/","success":true,"partial":false,"nominal_accounts":{"account@example.com":{"success":true,"message":"Codex quota reset credit consumed"}}}
```

```text
GET /v0/resource/plugins/user-routing/quota/direct/reset
Authorization: Bearer <Codex tokens.access_token>
ChatGPT-Account-ID: <Codex tokens.account_id>   # 可选，省略时从 token 中读取
```

返回 `success` 和以邮箱（或 account ID）为键的 `accounts`。

```text
POST /v0/management/plugins/user-routing/quota/reset
{"auth_index":"<CPA auth index>"}
```

这是 CPA 原生管理接口，需要 CPA 管理密钥，成功后 CPA 会同时清除该账号的本地冷却状态。

共同注意事项：

- 消耗请求**只发送一次，绝不自动重试**（之前的只读查询可以重试）。
- 如果连接在结果返回前超时，重置可能已经生效。请先重新查询剩余次数，不要直接再点一次。
- 前两种 GET 接口直接调用 Codex，CPA 不知道发生了重置，因此不会清除本地冷却；需要清除冷却时请用管理接口。

#### 额度缓存

```yaml
quota_provider:
  enabled: true          # 是否注册 QuotaProvider
  public_endpoint: true  # 是否开放上面这些 /v0/resource/... 接口；设为 false 只保留 CPA 原生管理接口
  cache_ttl: "1m"        # 成功的额度查询缓存时长，0s 表示关闭
```

- 按账号（`provider + auth_index`）缓存成功的查询结果；同一账号的并发查询会合并成一次。账号的认证内容变化后缓存自动失效。
- 查询失败或重置次数明细不完整时不缓存。
- 缓存中的某个额度窗口如果到了重置时间，会提前失效。
- 发起 Codex 重置前会清掉对应账号的缓存；`/quota/direct/reset` 因为不知道是 CPA 中哪个账号，会清掉所有 Codex 缓存。
- `/quota/direct` 永远不走缓存。
- 开启缓存后，额度数值和“实际前缀”的判断最多会滞后一个 `cache_ttl`。
- 如果名义前缀的账号被 CPA 标记为不可用，但 CPA 给出的 `next_retry_after` 已过，`/quota` 会重新检查它；没有重试时间的不可用账号仍会跳过。

Codex 的额度数据来自 ChatGPT 后台接口 `/backend-api/wham/usage`、`/backend-api/wham/rate-limit-reset-credits` 及其 `/consume`，参考 [Codex 官方客户端实现](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client/rate_limit_resets.rs)。这些接口不是公开 API，格式可能随时变化。

## 配置项参考

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 是否启用插件 |
| `prefix_map` | `{"default":""}` | API Key 到前缀的映射；`default` 是兜底前缀 |
| `strict_key_validation` | `true` | `prefix_map` 中出现 CPA 不认识的 Key 时拒绝加载配置 |
| `cpa_config_path` | 自动 | CPA 配置文件路径。依次尝试 CPA 的 `-config` 参数、环境变量 `CPA_CONFIG_PATH`、`./config.yaml` |
| `register_deduplicated_models` | `false` | 额外注册去掉前缀后的模型名 |
| `hide_prefixed_models` | `false` | 在模型列表中隐藏带前缀的条目，需要上一项开启 |
| `include_default_prefix` | `true` | 非空的 `default` 前缀是否也参与去前缀 |
| `quota_fallback.enabled` | `false` | 是否开启额度用完自动换组 |
| `quota_fallback.fallback_on_other_errors` | `false` | 其他上游错误是否也换组 |
| `quota_fallback.prefixes` | 空 | 每个前缀额度用完时依次尝试的前缀列表 |
| `quota_fallback.auto_sort.enabled` | `false` | 是否开启自动排序 |
| `quota_fallback.auto_sort.strategy` | `borrow` | 排序策略：`borrow`（借用时保护主人）或 `weekly_urgency`（优先用快作废的每周额度） |
| `quota_fallback.auto_sort.five_hour_reserve` | `25%` | `borrow` 策略的 5 小时保留线 |
| `quota_fallback.auto_sort.refresh_interval` | `5m` | 额度刷新间隔，最短 `30s` |
| `quota_fallback.auto_sort.prefixes` | 空 | 参与自动排序的前缀，开启时至少 2 个 |
| `quota_provider.enabled` | `true` | 是否注册 Codex 额度查询能力 |
| `quota_provider.public_endpoint` | `true` | 是否开放 `/v0/resource/plugins/user-routing/quota...` 接口 |
| `quota_provider.cache_ttl` | `1m` | 额度查询缓存时长，`0s` 关闭 |
| `models_url` | 自动 | CPA `/v1/models` 的完整地址，通常根据 CPA 的地址、端口、TLS 设置自动推导 |
| `model_cache_ttl` | `5s` | 模型列表缓存时长，`0s` 关闭 |
| `model_lookup_timeout` | `3s` | 查询模型列表的超时时间 |
| `models_tls_insecure_skip_verify` | `false` | 仅当 CPA 本机 HTTPS 使用自签名证书时开启 |
| `log_routing` | `true` | 是否在 CPA 日志中记录最终模型 |

关于 API Key 列表：旧版 CPA 读取顶层 `api-keys`；CPA v8 读取 `access.api-keys`，v8 顶层的 `api-keys` 是上游服务商的 Key，不会被当成客户端 Key。

## 已知问题

- Claude 的 token 计数接口不经过本插件；图像接口需要 CPA v8.0.5+，详见[支持的接口](#支持的接口)。

## 从源码构建

需要 Go 1.26+ 和目标平台的 C 编译器（插件使用 cgo 的 `c-shared` 模式）。

```bash
# Linux
./scripts/build.sh
```

```powershell
# Windows（推荐 LLVM-MinGW；部分新版 MinGW GCC 生成的 PE BigObj 文件 Go 无法处理）
$env:CC = "x86_64-w64-mingw32-clang"
.\scripts\build.ps1

# 在 Windows 上交叉编译 Linux amd64（需要安装 Zig）
.\scripts\build.ps1 -Target linux
```

产物在 `dist/` 目录。正式发布的版本号通过 `-ldflags "-X main.pluginVersion=<版本号>"` 注入。

### 发布

推送 `v*` 标签会触发 GitHub Actions：先跑测试，再构建 Windows、Linux、macOS 的 `amd64` 与 `arm64` 版本。每个平台单独打成一个 ZIP，并附带 `.sha256` 文件和汇总的 `checksums.txt`。

```bash
git tag v0.2.3
git push main v0.2.3
```

## 相关资料

- [CLIProxyAPI 插件开发文档](https://help.router-for.me/plugin/development)
- [CLIProxyAPI 模型路由器文档](https://help.router-for.me/plugin/model-router)
- [CLIProxyAPI 官方插件商店](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)
- [CPA Key Policy](https://github.com/origin652/cpa-plugin-key-policy)
