// payload_diag.go — TRAE_DEBUG_PAYLOAD=1 时的请求指纹诊断（issue #13，2026-09-23）。
//
// 背景：同一模型同一凭据，客户端只差 stream 标志，SOLO 通道非流式必挂流内
// 4001 而流式正常（报告者三凭据/两模型稳定复现）。插件侧两条执行链共用
// ChatStream → PrepareBody（确定性白名单），宿主侧 v7.2.30 源码两条 RPC 路径
// 传的 Payload 也是同一份 rawJSON——静态分析推不出分歧点，报告者建议在两个
// 入口各打一行 payload diff 实测。本文件就是那个开关：
//
//	TRAE_DEBUG_PAYLOAD=1 cpa run config.yaml
//
// 每次聊天请求在日志里落两行：
//
//	trae payload-diag: entry=execute|stream model=... raw sha256:... len=... client_stream=... messages=N
//	trae payload-diag: uid=... variant=... prepared sha256:... len=... function=... config_name=... stream=... max_tokens=... raw sha256:...
//
// entry 行来自两个执行器入口（executor.execute / executor.execute_stream，
// 互斥——一次请求只走其一），prepared 行来自 ChatStream 出站前。两行的
// raw 指纹相同即可对齐同一次请求；把一次失败的非流式调用与一次成功的
// 流式调用的 prepared 指纹对比：相同 ⇒ 出站请求逐字节一致，分歧在插件外
// （宿主构造/上游状态），插件侧无可修；不同 ⇒ 指纹行内的结构字段直接给出
// 分歧字段。指纹是 SHA-256 前 12 位 + 字节长度，不可逆，不含消息内容与
// 任何凭据材料。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// PayloadDiagEnabled 报告宿主进程环境是否开启请求指纹诊断。
// 插件以 c-shared 在宿主进程内运行，读的是 CPA 进程的环境变量。
func PayloadDiagEnabled() bool {
	return os.Getenv("TRAE_DEBUG_PAYLOAD") != ""
}

// Fingerprint 返回请求体的短指纹（SHA-256 前 12 hex + 字节长度）。
// 指纹相同 ⇔ 请求体逐字节相同。
func Fingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%s len=%d", hex.EncodeToString(sum[:])[:12], len(body))
}

// LogChatHead 在执行器入口处记录原始宿主 Payload 的指纹与结构摘要。
// entry 是 "execute"（同步聚合）或 "stream"（异步流式）；一次请求只经过
// 其中一个入口，所以 raw 指纹可以和 ChatStream 的 prepared 行对齐。
func LogChatHead(entry, model string, raw []byte) {
	if !PayloadDiagEnabled() {
		return
	}
	peek := struct {
		Stream   *bool             `json:"stream"`
		Messages []json.RawMessage `json:"messages"`
	}{}
	_ = json.Unmarshal(raw, &peek)
	clientStream := peek.Stream != nil && *peek.Stream
	log.Printf("trae payload-diag: entry=%s model=%q raw %s client_stream=%v messages=%d",
		entry, model, Fingerprint(raw), clientStream, len(peek.Messages))
}

// LogPreparedHead 在 ChatStream 出站前记录 PrepareBody 产物的指纹与白名单
// 字段值——这正是上游实际收到的请求内容。variant/config_name/function/
// max_tokens 是历史上引发流内 4001 的全部已知字段（v0.12.79 function、
// 命名空间后缀 config_name、v0.12.48 max_tokens 默认），一次对比即可定位。
func LogPreparedHead(uid, variant string, raw, prepared []byte) {
	if !PayloadDiagEnabled() {
		return
	}
	peek := struct {
		Function        string `json:"function"`
		ConfigName      string `json:"config_name"`
		Model           string `json:"model"`
		Stream          *bool  `json:"stream"`
		MaxTokens       any    `json:"max_tokens"`
		ReasoningEffort string `json:"reasoning_effort"`
	}{}
	_ = json.Unmarshal(prepared, &peek)
	log.Printf("trae payload-diag: uid=%s variant=%s prepared %s function=%q config_name=%q model=%q stream=%v max_tokens=%v reasoning_effort=%q raw %s",
		uid, variant, Fingerprint(prepared), peek.Function, peek.ConfigName, peek.Model,
		peek.Stream != nil && *peek.Stream, peek.MaxTokens, peek.ReasoningEffort, Fingerprint(raw))
}
