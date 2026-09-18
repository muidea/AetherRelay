package proxy

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	archive "aetherrelay/internal/pkg/aetherrelayarchive"
)

// clientResponseDebugInfo 是 response.meta.json 的内容,与 requestDebugInfo
// 对称,记录网关实际写给客户端的响应状态与 header。
//
// 它刻意与 metadata.json 的 http_status 分开:后者是 usage 结算用的业务状态,
// 这里记录的是 HTTP 层真正定型的状态码。两者不一致时(例如响应已写出、
// 之后才发现流式失败)可用于对照排障。
type clientResponseDebugInfo struct {
	RoundID int                 `json:"round_id"`
	At      time.Time           `json:"at"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	// Hijacked 为真表示该响应的 101 由升级方直接写到底层连接，status 与
	// headers 均不含握手响应行本身的内容。
	Hijacked bool `json:"hijacked,omitempty"`
}

// archiveResponseWriter 在响应定型时捕获实际写出的状态码与 header。
// 响应写出点分散在数十个 handler 中,在 ResponseWriter 层统一快照是唯一不会
// 漏点的位置。
//
// 必须透传 Unwrap / Flush / Hijack:SSE 依赖 http.Flusher 断言,WebSocket
// 升级依赖 http.Hijacker 断言,遮蔽其中任何一个都会让对应链路静默退化。
type archiveResponseWriter struct {
	http.ResponseWriter
	round *archive.Round
	// unredactedHeaders 是 CP-OBS-009 的请求级快照:客户端响应 header 与其余三类
	// 信息共用同一保真开关。
	unredactedHeaders bool
}

func (w *archiveResponseWriter) WriteHeader(status int) {
	w.capture(status, false)
	w.ResponseWriter.WriteHeader(status)
}

// Write 覆盖未显式调用 WriteHeader 的隐式 200 路径。
func (w *archiveResponseWriter) Write(body []byte) (int, error) {
	w.capture(http.StatusOK, false)
	return w.ResponseWriter.Write(body)
}

// Unwrap 让 http.ResponseController 能发现底层 ResponseWriter 的可选接口。
func (w *archiveResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush 透传刷写。底层（magicEngine 与 net/http）会在此时补写隐式 200，
// 但那次 WriteHeader 不会回流到本包装器，因此这里必须自己补一次快照，
// 否则「只 Flush 不 Write」的路径会缺失响应归档。
func (w *archiveResponseWriter) Flush() {
	w.capture(http.StatusOK, false)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack 透传协议升级。WebSocket 的 101 握手响应由升级方直接写到底层连接,
// 不经过 WriteHeader,因此这里在成功后补记 101,避免该轮对话缺失响应方向。
// gorilla 生成的 Upgrade / Sec-WebSocket-Accept 等握手头无法从 w 读到,
// 归档只包含升级前已由网关设置的 header。
func (w *archiveResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying response writer does not support hijacking: %w", http.ErrNotSupported)
	}
	conn, buffer, err := hijacker.Hijack()
	if err == nil {
		w.capture(http.StatusSwitchingProtocols, true)
	}
	return conn, buffer, err
}

// capture 在响应首次定型时冻结快照。
// Round.SetClientResponse 只记第一次,因此这里重复调用是安全的;先做一次读检查
// 是为了让流式响应的高频 Write 不必每次都重新构造脱敏 map。
func (w *archiveResponseWriter) capture(status int, hijacked bool) {
	if w == nil || w.round == nil {
		return
	}
	if _, captured := w.round.ClientResponse(); captured {
		return
	}
	w.round.SetClientResponse(status, archiveHeaderProjection(w.Header(), w.unredactedHeaders), hijacked)
}

// archiveClientResponse 把客户端响应 header 落到 response.meta.json。
// 正常路径由 writeArchiveMetadata 调用;客户端取消等不写 metadata.json 的早退
// 路径(handler.go 中 recordAndPrint 后直接 return 的分支)依赖 ServeHTTP
// 注册的 defer 兜底调用。
func (h *Handler) archiveClientResponse(round *archive.Round) {
	if round == nil || round.HasFile("response.meta.json") {
		return
	}
	response, captured := round.ClientResponse()
	if !captured {
		return
	}
	info := clientResponseDebugInfo{
		RoundID:  round.ID,
		At:       response.At,
		Status:   response.Status,
		Headers:  response.Headers,
		Hijacked: response.Hijacked,
	}
	if err := round.WriteJSON("response.meta.json", info); err != nil {
		log.Printf("archive client response metadata: %v", err)
	}
}
