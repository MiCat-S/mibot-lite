package acron

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// taskTypes 是 V2 的八种任务，顺序也照 V2。
var taskTypes = []string{"send", "copy", "forward", "del", "del_re", "pin", "unpin", "cmd"}

var typeLabels = map[string]string{
	"send": "发送", "cmd": "命令", "copy": "复制", "forward": "转发",
	"del": "删除", "del_re": "正则删除", "pin": "置顶", "unpin": "取消置顶",
}

func isTaskType(value string) bool {
	_, ok := typeLabels[value]
	return ok
}

// 任务的 delivery：执行前记成 prepared，有了结果再改成 sent 或 pending。
const (
	deliveryPending  = "pending"
	deliveryPrepared = "prepared"
	deliverySent     = "sent"
)

// task 是一个定时任务。字段名和取值照 MiBox V2 的 assets/acron/acron_config.json：
// 编号、时间戳（毫秒）、消息 ID、条数都存成字符串，--import-mibox 原样搬过来就能用，
// 这边写出去的 V2 也读得回。
type task struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Cron string `json:"cron"`
	// Chat 是用户写的对话（数字 ID、@用户名或 me），ChatID 是解析出来的带标记 ID。
	// 添加时解析不出来就先空着，第一次执行时再解析，ResolvedPeer 记下解析过了。
	Chat         string `json:"chat"`
	ChatID       string `json:"chatId,omitempty"`
	ResolvedPeer bool   `json:"resolvedPeer,omitempty"`
	CreatedAt    string `json:"createdAt"`
	LastRunAt    string `json:"lastRunAt,omitempty"`
	LastResult   string `json:"lastResult,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
	Remark       string `json:"remark,omitempty"`
	// Display 是添加时对话的显示名（HTML）。只为和 V2 的数据一致，列表按缓存现查。
	Display string `json:"display,omitempty"`
	// Message 是 send 要发的文字，或 cmd 要执行的命令。
	Message string `json:"message,omitempty"`
	// Entities 是 send 那段文字的格式，写法是 teleproto 的 TL JSON，见 encodeEntities。
	// 存原样的 JSON：读进来再写回去时，认不得的字段也不会丢。
	Entities json.RawMessage `json:"entities,omitempty"`
	// ReplyTo 是发送时回复的消息（论坛里写话题的首条消息就进这个话题）；forward 只能是话题。
	ReplyTo    string `json:"replyTo,omitempty"`
	FromChatID string `json:"fromChatId,omitempty"`
	FromMsgID  string `json:"fromMsgId,omitempty"`
	MsgID      string `json:"msgId,omitempty"`
	Limit      string `json:"limit,omitempty"`
	Regex      string `json:"regex,omitempty"`
	Notify     bool   `json:"notify,omitempty"`
	PmOneSide  bool   `json:"pmOneSide,omitempty"`
	// Delivery 见 deliveryPrepared：启动后看到 prepared，说明上次执行到一半进程没了，
	// 那次到底生效没有不知道，下一次就跳过，免得重复发、重复删。
	Delivery string `json:"delivery,omitempty"`
}

// state 是整个数据文件 data/acron.json。
type state struct {
	SchemaVersion int    `json:"schemaVersion"`
	Seq           string `json:"seq"`
	Tasks         []task `json:"tasks"`
}

func defaults() state { return state{SchemaVersion: 1, Seq: "0", Tasks: []task{}} }

// UnmarshalJSON 读任务时把写成数字的 id、chatId 当字符串读。V2 每次启动都会把旧数据里的
// 这两项转成字符串再写回去；没被 V2 转过的文件，这里做同样的事。
func (t *task) UnmarshalJSON(raw []byte) error {
	type plain task
	fixed, err := quoteNumbers(raw, "id", "chatId")
	if err != nil {
		return err
	}
	return json.Unmarshal(fixed, (*plain)(t))
}

// UnmarshalJSON 同 task 的，seq 也可能是数字。
func (s *state) UnmarshalJSON(raw []byte) error {
	type plain state
	fixed, err := quoteNumbers(raw, "seq")
	if err != nil {
		return err
	}
	return json.Unmarshal(fixed, (*plain)(s))
}

// quoteNumbers 把对象里 keys 这几项的数字值改写成字符串，别的原样。
func quoteNumbers(raw []byte, keys ...string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	changed := false
	for _, key := range keys {
		value := bytes.TrimSpace(fields[key])
		if len(value) > 0 && (value[0] == '-' || value[0] >= '0' && value[0] <= '9') {
			fields[key] = json.RawMessage(strconv.Quote(string(value)))
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(fields)
}

// normalize 补上 V2 启动时会补的默认值。只改读出来的副本，不写回文件：--check 时不能动 data/。
func (s *state) normalize() {
	if s.SchemaVersion == 0 {
		s.SchemaVersion = 1
	}
	if s.Seq == "" {
		s.Seq = "0"
	}
	if s.Tasks == nil {
		s.Tasks = []task{}
	}
	for index := range s.Tasks {
		if s.Tasks[index].Delivery == "" {
			s.Tasks[index].Delivery = deliveryPending
		}
	}
}

func (s state) find(id string) int {
	for index := range s.Tasks {
		if s.Tasks[index].ID == id {
			return index
		}
	}
	return -1
}

// nextID 是新任务的编号：seq 加一，和 V2 一样。seq 读不出来（手改坏了）时从现有编号里最大的往上数，
// 再跳过已经被占用的。
func (s state) nextID() string {
	next, err := strconv.ParseUint(s.Seq, 10, 64)
	if err != nil {
		next = 0
		for _, t := range s.Tasks {
			if value, err := strconv.ParseUint(t.ID, 10, 64); err == nil && value > next {
				next = value
			}
		}
	}
	for {
		next++
		id := strconv.FormatUint(next, 10)
		if s.find(id) < 0 {
			return id
		}
	}
}

// messageID 读一个存成字符串的消息 ID，不是正整数时返回 0。
func messageID(value string) int {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// splitTarget 拆开「对话|消息 ID」，竖线也可以是全角的，同 V2。
func splitTarget(value string) (chat, replyTo string) {
	var parts []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '|' || r == '｜' }) {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) > 0 {
		chat = parts[0]
	}
	if len(parts) > 1 {
		replyTo = parts[1]
	}
	return chat, replyTo
}

// remarkAfter 去掉 line 开头的 n 个词，剩下的是备注；备注中间的空白原样保留，同 V2。
func remarkAfter(line string, n int) string {
	rest := line
	for count := 0; count < n; count++ {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if end < 0 {
			return ""
		}
		rest = rest[end:]
	}
	return strings.TrimSpace(rest)
}

// parseFlag 读 pin 的两个开关：1、true、yes、y 是开，其余都是关，同 V2。
func parseFlag(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}

func flagText(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// copyCommand 拼出能重新添加这个任务的命令，同 V2 的 buildCopy。cmd 的命令在第二行。
func copyCommand(t task, prefix string) string {
	remark := ""
	if t.Remark != "" {
		remark = " " + t.Remark
	}
	reply := ""
	if t.ReplyTo != "" {
		reply = "|" + t.ReplyTo
	}
	head := prefix + "acron " + t.Type + " " + t.Cron + " " + t.Chat
	switch t.Type {
	case "send", "copy", "forward":
		return head + reply + remark
	case "cmd":
		return head + reply + remark + "\n" + t.Message
	case "del", "unpin":
		return head + " " + t.MsgID + remark
	case "del_re":
		return head + " " + t.Limit + " " + t.Regex + remark
	case "pin":
		return head + " " + t.MsgID + " " + flagText(t.Notify) + " " + flagText(t.PmOneSide) + remark
	}
	return prefix + "acron"
}

// copyHTML 是 copyCommand 的显示：多行的放进 <pre>，单行的放进 <code>。
func copyHTML(t task, prefix string) string {
	text := copyCommand(t, prefix)
	if strings.Contains(text, "\n") {
		return "<pre>" + command.Escape(text) + "</pre>"
	}
	return command.Code(text)
}

// shanghai 是任务的时区。V2 把任务按 Asia/Shanghai 注册，数据里的 Cron 都是按北京时间写的。
var shanghai = mustLocation("Asia/Shanghai")

func mustLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return location
}

// formatTime 按北京时间写日期时间；Cron 精确到秒，秒不是 0 时才写出来。
func formatTime(at time.Time) string {
	local := at.In(shanghai)
	if local.Second() != 0 {
		return local.Format("2006-01-02 15:04:05")
	}
	return local.Format("2006-01-02 15:04")
}

// millis 是 V2 存时间的写法：毫秒时间戳的十进制字符串。
func millis(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) }

// parseMillis 读 millis 存的时间，也认 RFC3339。
func parseMillis(value string) (time.Time, bool) {
	if ms, err := strconv.ParseInt(value, 10, 64); err == nil && ms > 0 {
		return time.UnixMilli(ms), true
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, true
	}
	return time.Time{}, false
}

// peerOf 把 input peer 换回 peer，收藏夹（InputPeerSelf）换成本账号。
func peerOf(input tg.InputPeerClass, selfID int64) tg.PeerClass {
	switch value := input.(type) {
	case *tg.InputPeerUser:
		return &tg.PeerUser{UserID: value.UserID}
	case *tg.InputPeerChat:
		return &tg.PeerChat{ChatID: value.ChatID}
	case *tg.InputPeerChannel:
		return &tg.PeerChannel{ChannelID: value.ChannelID}
	case *tg.InputPeerSelf:
		return &tg.PeerUser{UserID: selfID}
	}
	return nil
}

// chatLabel 是列表里对话的显示：缓存里有名字的写「名字（对话）」，本账号写收藏夹，
// 都没有就只写用户当初写的对话。V2 每次列表都联网查一遍，这里只查缓存。
func chatLabel(client *bot.Client, t task) string {
	fallback := command.Code(t.Chat)
	if client == nil || t.ChatID == "" {
		return fallback
	}
	peer, ok := bot.PeerFromID(t.ChatID)
	if !ok {
		return fallback
	}
	if user, ok := peer.(*tg.PeerUser); ok && user.UserID == client.SelfID() {
		return "收藏夹"
	}
	title := client.Peers().Title(peer)
	if title == "" || title == t.ChatID {
		return fallback
	}
	return command.Escape(title) + "（" + command.Code(t.Chat) + "）"
}

// messageLink 是消息 ID 的显示：频道和超级群的消息做成链接，其余只写 ID。
func messageLink(chatID, id string) string {
	if channel, ok := strings.CutPrefix(chatID, "-100"); ok && channel != "" && messageID(id) > 0 {
		return `<a href="https://t.me/c/` + command.Escape(channel) + "/" + command.Escape(id) + `">` + command.Escape(id) + "</a>"
	}
	return command.Code(id)
}
