# cpa-multi-plugins

> CPA (CLIProxyAPI) 动态库插件集合：CodeBuddy / WorkBuddy / Trae / Qoder 的 CN + Intl 版本

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macos%20%7C%20windows-lightgrey)]()
[![Status](https://img.shields.io/badge/status-WIP-orange)]()

## 项目目标

为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 提供完整的国内 AI IDE 平台 provider 插件，让 CPA 一个 `/v1/chat/completions` 接口就能调用所有模型。

## 插件清单

| 插件 | 平台 | 协议 | 签到 | 状态 |
|---|---|---|---|---|
| `codebuddy-cn` | CodeBuddy CN (`copilot.tencent.com`) | OpenAI 兼容 | ✅ 每日 | ✅ Built |
| `codebuddy-intl` | CodeBuddy Intl (`codebuddy.ai`) | OpenAI 兼容 | ❌ | ✅ Built |
| `workbuddy` | WorkBuddy (Tencent 办公版) | OpenAI 兼容 | ✅ 每日 | ✅ Built |
| `trae-intl` | Trae Intl (`api.marscode.com`) | 私有 SSE | ❌ | ✅ Built |
| `trae-cn` | Trae Code CN (`api.trae.cn`) | 私有 SSE + v2 积分 | ✅ 每日 | ✅ Built |
| `trae-solo-cn` | Trae Work CN / SOLO CN | 私有 SSE + v2 积分 | ✅ 每日 | ✅ Built |
| `qoder-intl` | Qoder Intl (`qoder.com`) | COSY 签名 | ❌ | ✅ Built |
| `qoder-cn` | QoderWork CN (`qoder.com.cn`) | COSY 签名 | ✅ 每日 | ✅ Built |

## 协议复用

基于代码事实，3 套核心实现覆盖 8 个 provider：

```
codebuddy-core  →  codebuddy-cn / codebuddy-intl / workbuddy
trae-core       →  trae-intl / trae-cn / trae-solo-cn
qoder-core      →  qoder-intl / qoder-cn
```

每个 provider 是一个 thin wrapper，传入不同配置（host / client_id / function / has_checkin）即可。

## 安装

### 1. 启用 CPA 插件

`config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    codebuddy-cn: { enabled: true }
    codebuddy-intl: { enabled: true }
    workbuddy: { enabled: true }
    trae-cn: { enabled: true }
    trae-solo-cn: { enabled: true }
    qoder-cn: { enabled: true }
```

### 2. 下载 .so/.dll/.dylib

从 [Releases](../../releases) 下载对应平台的二进制，放到 CPA 的 `plugins/` 目录。

### 3. 登录账号

通过 CPA 的 Management API 或 CPAMP 面板触发 OAuth 登录流程。

## 构建要求

- Go 1.26+
- CGO 启用
- 目标平台 C 编译器（gcc / clang / msvc）

```bash
# 编译所有插件（Linux amd64 示例）
make all-linux-amd64

# 编译单个插件
cd plugins/codebuddy-cn && make
```

## 协议参考

本项目基于以下开源项目的协议事实实现（不直接抄代码，仅参考协议层）：

- [Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api) — Trae SOLO CN 反代（Go）
- [Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin) — WorkBuddy + QoderWork 现有 CPA 插件
- [HanHan666666/codebuddy2openai](https://github.com/HanHan666666/codebuddy2openai) — CodeBuddy CN 转 OpenAI
- [1416277987/proxy-hub](https://github.com/1416277987/proxy-hub) — 多平台反代
- [jlcodes99/cockpit-tools](https://github.com/jlcodes99/cockpit-tools) — 16 平台账号管理（协议事实层参考）
- [decolua/9router](https://github.com/decolua/9router) — Trae Intl JS 实现
- [diegosouzapw/OmniRoute](https://github.com/diegosouzapw/OmniRoute) — Trae/Qoder TS 实现
- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) — CPA 插件 SDK

详细协议事实清单见 [docs/PROTOCOL.md](docs/PROTOCOL.md)。

## License

MIT — 详见 [LICENSE](LICENSE)

## 致谢

- CPA 项目作者 [router-for-me](https://github.com/router-for-me)
- traework2api / cpa-plugin 作者 [Sliverkiss](https://github.com/Sliverkiss)
- cockpit-tools 作者 [jlcodes99](https://github.com/jlcodes99)
- 所有协议层逆向工程贡献者

## Status

✅ **8/8 plugins built (5 functional, 3 skeleton)** — 正在开发中，参见 [Issues](../../issues) 跟踪进度。
