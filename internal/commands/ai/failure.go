package ai

import (
	"errors"
	"fmt"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// RequestInfo 是失败日志需要的请求信息；sum 自己的服务商也用它。
type RequestInfo struct {
	Tag, Model        string
	Stream, Responses bool
	Timeout           time.Duration
	Stage             string
}

// Failure 保留原因和请求信息，聊天里仍然只显示原有的简短说明。
func (info RequestInfo) Failure(err error, started time.Time, secret string) error {
	if err == nil {
		return nil
	}
	stage := info.Stage
	var requestErr *httpx.RequestError
	if errors.As(err, &requestErr) {
		stage = requestErr.Stage
	}
	detail := fmt.Sprintf("AI request tag=%q model=%q stream=%t responses=%t timeout=%s took=%s stage=%s: %s",
		info.Tag, info.Model, info.Stream, info.Responses, info.Timeout, time.Since(started), stage, err)
	return requestFailure{cause: err, detail: maskSecret(detail, secret)}
}

type requestFailure struct {
	cause  error
	detail string
}

func (e requestFailure) Error() string { return e.detail }
func (e requestFailure) Unwrap() error { return e.cause }
func (e requestFailure) UserMessage() string {
	if text, ok := kit.IsUserError(e.cause); ok {
		return text
	}
	return "AI 请求失败：" + httpx.Reason(e.cause)
}

func (r *aiRequest) info() RequestInfo {
	stream, _ := r.Body["stream"].(bool)
	return RequestInfo{Tag: r.Provider.Tag, Model: r.Model, Stream: stream, Responses: r.Responses, Timeout: r.Timeout, Stage: "decode"}
}
