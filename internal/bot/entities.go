package bot

import (
	"context"
	"reflect"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// EntityRecorder 是连接中间件：每个成功的应答里出现的用户和群都记进 Peers。
//
// MiBox 所用的 teleproto 对每个应答都这么做，之后凭 ID 就能找到见过的任何人或群。
// 以前这里只在读消息、查成员这些地方手动记，别的请求（比如发消息、转发返回的更新）
// 带回来的用户就漏掉了，之后按 ID 找会说「无法解析」。
type EntityRecorder struct {
	Peers *PeerCache
}

type userCarrier interface{ GetUsers() []tg.UserClass }
type chatCarrier interface{ GetChats() []tg.ChatClass }

// Handle 实现 telegram.Middleware。
func (r EntityRecorder) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		err := next.Invoke(ctx, input, output)
		if err == nil && r.Peers != nil {
			r.remember(output)
		}
		return err
	}
}

// remember 从应答里找出用户和群。gotd 把「多种可能的应答类型」装在只有一个字段的
// Box 结构里（如 MessagesMessagesBox{Messages: ...}），所以应答本身和 Box 里装的值都要看。
func (r EntityRecorder) remember(output bin.Decoder) {
	candidates := []any{output}
	if value := reflect.ValueOf(output); value.Kind() == reflect.Pointer && !value.IsNil() && value.Elem().Kind() == reflect.Struct {
		inner := value.Elem()
		for index := 0; index < inner.NumField(); index++ {
			field := inner.Field(index)
			if field.Kind() == reflect.Interface && !field.IsNil() && field.CanInterface() {
				candidates = append(candidates, field.Interface())
			}
		}
	}
	for _, candidate := range candidates {
		if users, ok := candidate.(userCarrier); ok {
			r.Peers.RememberUsers(users.GetUsers())
		}
		if chats, ok := candidate.(chatCarrier); ok {
			r.Peers.RememberChats(chats.GetChats())
		}
		if vector, ok := candidate.(*tg.UserClassVector); ok {
			r.Peers.RememberUsers(vector.Elems)
		}
	}
}
