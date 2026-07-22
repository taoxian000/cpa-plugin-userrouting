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

支持的 API/协议：

- OpenAI Chat Completions (`openai`)
- OpenAI Responses (`openai-response`)
- Claude Messages (`claude`)
- Gemini (`gemini`)
- OpenAI Videos：`POST /v1/videos`、`/v1/videos/generations`、`/v1/videos/edits`、`/v1/videos/extensions`，以及 `/openai/v1/videos`（`openai-video`）

视频请求与消息请求使用同一套 API Key 前缀、模型目录校验、日志与额度回退逻辑。Claude `/v1/messages/count_tokens` 不经过插件，因为当前 CLIProxyAPI 插件 ABI 没有“通过主程序执行 token count”的回调；该接口保留 CPA 原生行为。

图像和在线搜索端点当前不能由本插件安全路由，原因均在 CPA 主程序：图像端点的插件执行器回调没有传递“允许图像模型”标记，因此 CPA 会拒绝 `gpt-image-*`、`grok-imagine-image*` 等图像专用模型；`POST /v1/alpha/search` 和 `POST /backend-api/codex/alpha/search` 则会直接选择 Codex 鉴权并发起上游 HTTP 请求，未调用插件的模型路由器或执行器。两类功能都需要 CPA 主程序增加对应的插件回调；仅更新本动态插件无法实现。

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

推送匹配 `v*` 的 Git 标签会自动触发 GitHub Actions。工作流先运行测试，然后构建以下动态库：Windows、Linux、macOS 的 `amd64` 与 `arm64`，以及 FreeBSD `amd64`。

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
| `quota_fallback.enabled` | `false` | 是否在 Codex 账号返回 `usage_limit_reached` 时启用跨前缀模型回退 |
| `quota_fallback.fallback_on_other_errors` | `false` | 是否也在其他上游错误时进行跨前缀模型回退；流式请求仅在输出首个内容前回退 |
| `quota_fallback.prefixes` | 空 | 源前缀到按顺序尝试的目标前缀列表 |
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
