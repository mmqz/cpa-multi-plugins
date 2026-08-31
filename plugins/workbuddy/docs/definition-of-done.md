# WorkBuddy Plugin — Definition of "Done" / 完美指标

本文件是 v0.8.0 社区交付版的验收标准。每一项都必须 **量化可测**，
跑通验证脚本才能宣称完成。

## A. 代码质量（自动化可验）

| 指标 | 目标 | 测量方式 |
|---|---|---|
| `gofmt -l .` | 0 files | `gofmt -l . \| wc -l` = 0 |
| `go vet ./...` | 0 issues | exit code 0 |
| `gocritic check ./...` | 0 issues | exit 0 |
| `staticcheck ./...` (filtered) | 0 real issues | filter out toolchain-version noise |
| `unparam ./...` (optional) | 0 issues | requires go1.26-compatible build |
| `gocyclo -over 15 .` (optional) | ≤ 5 | requires go1.26-compatible build |
| 单文件行数 | ≤ 1500 行（不含 _test.go） | `wc -l *.go \| awk '$1>1500 && !/_test/'` = 0 |
| 单函数行数 | ≤ 200 行 | 手工抽 + 高复杂度函数豁免（OAuth/parse） |
| 圈复杂度平均 | ≤ 8 | 手工审查 |

**工具链说明（v0.8.0）**：`unparam`/`gocyclo`/`staticcheck` 的部分二进制
是 Go 1.25 编译的，无法分析 Go 1.26 代码（`iter` 包路径变更）。这些
工具的"新版本检查"是噪音，不是真实代码问题。我们用 `go vet`（Go 自带
1.26 兼容）+ `gocritic`（恰好兼容）作为强制扫描，其余作为可选。

## B. 测试（必须全绿）

| 指标 | 目标 |
|---|---|
| `go test ./...` | 100% pass |
| `go test -race ./...` | 100% pass |
| 测试覆盖率（语句） | ≥ 70%（`go test -cover`） |
| 关键路径覆盖（cache merge / alias 反解 / tool_choice 归一 / SSE 解析） | 100% |

## C. 性能（实测对比）

| 指标 | 目标 | 基线 |
|---|---|---|
| 非流式 chat 首 token | ≤ +5% vs v0.6.27 | 同账号同 prompt |
| 流式 chat TTFB | ≤ +5% vs v0.6.27 | 同账号同 prompt |
| 热路径 JSON 序列化次数 | 1 次/请求（unmarshal+marshal） | v0.6.27 是 4-5 次 |
| 面板 `/accounts` 响应（N=10账号，cache hit） | p95 ≤ 50ms | 实测 |
| 面板 `/accounts` 并发刷新（N=10账号，cache miss） | 上游调用次数 = N（singleflight 去重） | 实测 |

## D. 跨平台兼容

| 指标 | 目标 |
|---|---|
| `GOOS=linux GOARCH=amd64 go build -buildmode=c-shared` | 编译通过 |
| `GOOS=linux GOARCH=arm64 go build -buildmode=c-shared` | 编译通过 |
| `GOOS=darwin GOARCH=amd64 go build -buildmode=c-shared` | 编译通过 |
| `GOOS=darwin GOARCH=arm64 go build -buildmode=c-shared` | 编译通过 |
| `GOOS=windows GOARCH=amd64 go build -buildmode=c-shared` | 编译通过 |
| CGO 依赖 | 仅 c-shared 必需，无外部 C 库依赖 |
| `go.mod` Go 版本 | 与 CPA 宿主一致（当前 1.26） |

## E. 文档（社区标准）

| 文件 | 必须包含 |
|---|---|
| `README.md` | Features / Quickstart / Configuration / Lifecycle / Development / License |
| `README_CN.md` | 中文版 |
| `CHANGELOG.md` | Keep a Changelog 格式，每版本日期 |
| `docs/architecture.md` | 模块图 + 数据流 + 关键设计决策 + 与 CPA 的集成点 |
| `docs/development.md` | 本地构建 / 测试 / 调试 / 发布流程 |
| `LICENSE` | MIT |
| `.gitignore` | 忽略 `*.so`、`*.h`、`bin/`、`dist/` |
| `Makefile` | `make build` / `make test` / `make lint` / `make clean` / `make release` |

## F. 安全（红线）

| 检查项 | 目标 |
|---|---|
| `go list -deps . \| xargs gosec` | 0 high/critical |
| 硬编码 secret | 0 |
| 路径穿越 | UID 白名单 + `isSafeWorkbuddyAuthPath` 双保险 |
| 敏感日志 | `redactSecrets` 覆盖 Bearer/JWT/kv/裸 JWT 四种形态 |
| 鉴权 | 插件层 constant-time Bearer + per-IP token bucket |
| 前端 | 无 innerHTML 注入用户/上游可控字段进 JS 上下文 |

## G. 功能实测（必须通过）

| 场景 | 通过标准 |
|---|---|
| 非流式 chat (CN 账号) | 200 + valid completion + usage 完整 |
| 流式 chat (CN 账号) | SSE chunks + [DONE] + usage 完整 |
| 非流式 chat (Global 账号) | 200（Global 需要注入 system msg） |
| 流式 chat (Global 账号) | SSE 正常 |
| OAuth 登录 (CN) | 完整 flow 出 token + auth 文件落盘 |
| 签到 (CN) | 200 + today_checked_in=true + 面板显示已签到 |
| Trial 领取 (Global) | 200 + trial_claimed=true |
| Credits 查询 | 200 + packages 数组完整 |
| 耗尽账号调度 | 切换到非耗尽账号 |
| 面板载入 | 0 JS error + 0 `parse failed` |
| CPAMP usage 上报 | 请求后 5s 内 CPAMP `/v0/management/usage` 出现该记录 |
| 多平台编译 | D 节全部通过 |

## H. 兼容性（向后）

| 检查项 | 目标 |
|---|---|
| CPA v7.2.30+ | 全部功能正常 |
| 配置文件向后兼容 | 旧 `config_yaml` 字段不报错 |
| Auth 文件向后兼容 | 旧 `workbuddy-<uid>.json` 可读 |
| 面板 API 响应 schema | 字段不删，只增 |

---

**验收流程**：
1. 全部自动化指标跑通（A/B/D/F）
2. 性能实测报告归档 `docs/benchmarks/v0.8.0.md`（C）
3. 功能实测报告归档 `docs/e2e/v0.8.0.md`（G）
4. 文档齐全（E）
5. CHANGELOG 写明 breaking changes（H）

任何一项不达标 → 回到重构，直至全部通过。
