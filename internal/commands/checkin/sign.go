package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

var (
	// replyWait 是等对方回复的最长时间，pollEvery 是隔多久读一次对话，gap 是两个目标之间停多久。
	// 测试里改短。
	replyWait = 10 * time.Second
	pollEvery = time.Second
	gap       = 2 * time.Second
	// botAPI 是 Bot API 的地址，测试里换成本地的假服务。
	botAPI = "https://api.telegram.org"
)

// signOne 签到一个目标：发签到命令，等对方回复；要点按钮的，在回复里找到按钮点一下。
// 返回对方的回复（点按钮的是按钮的答复），失败时的错误是给用户看的一句话。
func signOne(ctx context.Context, client *bot.Client, target signTarget) (string, error) {
	peer, err := client.ResolveTarget(ctx, target.Target)
	if err != nil {
		return "", kit.FailWith("找不到目标 "+target.Target, err)
	}
	// 签到命令原样发出去，不给 IP 打码：命令里的数字被改了就签不上。
	sent, err := client.SendText(bot.WithoutIPPrivacy(ctx), peer, target.Command, bot.SendOptions{})
	if err != nil {
		return "", kit.FailWith("签到命令发送失败", err)
	}
	wantsButton := target.CallbackData != "" || target.ButtonText != ""
	reply, err := awaitMessage(ctx, client, peer, func(message *tg.Message) bool {
		if message.Out || message.ID <= sent {
			return false
		}
		_, found := buttonData(message, target)
		return found || !wantsButton
	})
	switch {
	case err != nil:
		return "", err
	case reply == nil && wantsButton:
		return "", kit.Failf("%d 秒内没收到带这个按钮的回复（%s）", int(replyWait.Seconds()), buttonLabel(target))
	case reply == nil:
		return "", kit.Failf("%d 秒内没收到回复", int(replyWait.Seconds()))
	case !wantsButton:
		return reply.Message, nil
	}
	data, _ := buttonData(reply, target)
	return click(ctx, client, peer, reply, data)
}

func buttonLabel(target signTarget) string {
	if target.CallbackData != "" {
		return "回调数据 " + target.CallbackData
	}
	return "按钮文字 " + target.ButtonText
}

// buttonData 在消息的内联键盘里找目标要点的回调按钮，返回它的回调数据。
// 设了回调数据的按回调数据找，否则按按钮上的文字找。
func buttonData(message *tg.Message, target signTarget) ([]byte, bool) {
	markup, ok := message.ReplyMarkup.(*tg.ReplyInlineMarkup)
	if !ok {
		return nil, false
	}
	for _, row := range markup.Rows {
		for _, button := range row.Buttons {
			callback, ok := button.Type.(*tg.InlineButtonTypeCallback)
			if !ok {
				continue
			}
			if target.CallbackData != "" && string(callback.Data) == target.CallbackData ||
				target.CallbackData == "" && target.ButtonText != "" && button.Text == target.ButtonText {
				return callback.Data, true
			}
		}
	}
	return nil, false
}

// click 点 message 上回调数据是 data 的按钮。机器人的答复（弹出来的那句话）就是结果；
// 没有的话，等它接下来发的消息，或者它把带按钮的那条改成的结果。
func click(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, message *tg.Message, data []byte) (string, error) {
	request := &tg.MessagesGetBotCallbackAnswerRequest{Peer: peer, MsgID: message.ID}
	request.SetData(data)
	answer, err := client.API().MessagesGetBotCallbackAnswer(ctx, request)
	switch {
	case tgerr.Is(err, "BOT_RESPONSE_TIMEOUT"):
		// 按钮点到了，只是机器人没在时限里答复；结果看它接下来发什么。
	case err != nil:
		return "", kit.FailWith("点按钮失败", err)
	case answer.Message != "":
		return answer.Message, nil
	}
	edited := message.EditDate
	reply, err := awaitMessage(ctx, client, peer, func(candidate *tg.Message) bool {
		return !candidate.Out && (candidate.ID > message.ID || candidate.ID == message.ID && candidate.EditDate > edited)
	})
	switch {
	case err != nil:
		return "", err
	case reply == nil:
		return "已点按钮，没收到回复", nil
	}
	return reply.Message, nil
}

// awaitMessage 每隔 pollEvery 读一次对话最近的 8 条，直到有 match 认可的消息或者等满 replyWait。
// 有几条都认可时取最早的那条：它才是对签到命令的回复。等满了没有返回 nil。
func awaitMessage(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, match func(*tg.Message) bool) (*tg.Message, error) {
	deadline := time.Now().Add(replyWait)
	for time.Now().Before(deadline) {
		if err := kit.Sleep(ctx, pollEvery); err != nil {
			return nil, err
		}
		result, err := client.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 8})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			client.Logger().Debug("checkin.poll_failed", "error", err.Error())
			continue
		}
		messages, _ := client.Unpack(result)
		var found *tg.Message
		for _, item := range messages {
			if message, ok := item.(*tg.Message); ok && match(message) && (found == nil || message.ID < found.ID) {
				found = message
			}
		}
		if found != nil {
			return found, nil
		}
	}
	return nil, nil
}

// runAll 依次签到 targets，每两个之间停 gap，返回汇总（HTML）。source 是「手动」或「定时」。
func runAll(ctx context.Context, client *bot.Client, targets []signTarget, source string) string {
	started := time.Now()
	var lines []string
	succeeded := 0
	for index, target := range targets {
		if index > 0 {
			_ = kit.Sleep(ctx, gap)
		}
		reply, err := signOne(ctx, client, target)
		if err != nil {
			client.Logger().Warn("checkin.failed", "target", target.ID, "error", err.Error())
			lines = append(lines, "❌ "+command.Escape(target.Name)+"："+command.Escape(reason(err)))
			continue
		}
		succeeded++
		lines = append(lines, "✅ "+command.Escape(target.Name)+"："+command.Escape(brief(reply)))
	}
	header := []string{"📅 <b>签到汇总</b>",
		source + " · " + started.In(shanghai).Format("2006-01-02 15:04") + "（北京时间）",
		fmt.Sprintf("成功 %d 个，失败 %d 个", succeeded, len(targets)-succeeded), ""}
	return strings.Join(append(header, lines...), "\n")
}

// reason 是签到失败时汇总里写的原因。
func reason(err error) string {
	if errors.Is(err, context.Canceled) {
		return "服务停了，没签完"
	}
	if text, ok := kit.IsUserError(err); ok {
		return text
	}
	return command.Brief(err)
}

// brief 把对方的回复压成一行，放进汇总。
func brief(reply string) string {
	reply = strings.Join(strings.Fields(reply), " ")
	if reply == "" {
		return "（回复里没有文字）"
	}
	return command.Truncate(reply, 80)
}

// deliver 发汇总：设了机器人推送先用它，其次汇总对话；都没有或者都发不出去时，手动签到改写
// 命令消息，定时签到发到收藏夹。汇总发到了别处的话，手动签到的命令消息删掉，和原插件一样。
func deliver(ctx context.Context, client *bot.Client, cfg config, summary string, inv *command.Invocation) error {
	pages := command.HTMLPages(summary, command.PageLimit)
	if cfg.BotToken != "" && cfg.PushChatID != "" {
		err := pushViaBot(ctx, cfg.BotToken, cfg.PushChatID, pages)
		if err == nil {
			return removeCommand(ctx, inv)
		}
		client.Logger().Warn("checkin.bot_push_failed", "error", err.Error())
	}
	if cfg.LogChat != "" {
		err := sendPages(ctx, client, cfg.LogChat, pages)
		if err == nil {
			return removeCommand(ctx, inv)
		}
		client.Logger().Warn("checkin.log_chat_failed", "chat", cfg.LogChat, "error", err.Error())
	}
	if inv != nil {
		return inv.EditPages(ctx, pages)
	}
	return sendPages(ctx, client, "me", pages)
}

func removeCommand(ctx context.Context, inv *command.Invocation) error {
	if inv == nil {
		return nil
	}
	return inv.Client.DeleteMessage(ctx, inv.Message)
}

func sendPages(ctx context.Context, client *bot.Client, chat string, pages []string) error {
	peer, err := client.ResolveTarget(ctx, chat)
	if err != nil {
		return err
	}
	for _, page := range pages {
		if _, err := client.SendHTML(ctx, peer, page, bot.SendOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// pushViaBot 用 Bot API 把汇总发到 chat。返回的错误里不带 URL：URL 里有 Token。
func pushViaBot(ctx context.Context, token, chat string, pages []string) error {
	for _, page := range pages {
		response, err := httpx.PostJSON(ctx, botAPI+"/bot"+token+"/sendMessage", nil,
			map[string]any{"chat_id": chat, "text": page, "parse_mode": "HTML", "disable_web_page_preview": true}, 15*time.Second, 1<<20)
		if err != nil {
			return errors.New(httpx.Reason(err))
		}
		if !response.OK() {
			var body struct {
				Description string `json:"description"`
			}
			_ = json.Unmarshal(response.Body, &body)
			return fmt.Errorf("Bot API 返回 HTTP %d：%s", response.Status, body.Description)
		}
	}
	return nil
}
