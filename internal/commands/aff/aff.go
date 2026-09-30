// Package aff 实现 .aff：保存几段推广（Affiliate）文字，需要时一条命令发出来。
//
// 数据格式和 MiBox 的 aff 插件（assets/aff/data.json）一样，--import-mibox 可以原样搬过来；
// TeleBox 原版只存一条的旧格式（aff 字段）第一次用到时并进列表，和 MiBox 一样。
package aff

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

const (
	// maxEntries、maxLength、pageSize 都和 MiBox 一样。
	maxEntries = 32
	// maxLength 是正文的长度上限（UTF-16 单位，按去掉格式后的文字算），给 Telegram 的
	// 4096 留出余地。
	maxLength = 4000
	pageSize  = 10
	// previewRunes 是列表里每条显示的字数。
	previewRunes = 30
)

// entry 是一条保存的文字。字段和 MiBox 的一样，三代数据的含义不同：
//   - 有 Format "html"：本程序和 MiBox v2 后期存的，按 HTML 发；
//   - 有 web_page：TeleBox 原版存的，也是 HTML；
//   - 只有 webPage：MiBox v2 早期存的纯文本，原样发，不当 HTML 解析。
//
// webPage 和 web_page 记的是正文里有没有链接，原版据此关掉链接预览。本程序编辑消息时
// 总是关掉预览（没有链接的正文本来也没有预览可显示），所以只为兼容照写。
type entry struct {
	Text          string `json:"text"`
	WebPage       *bool  `json:"webPage,omitempty"`
	LegacyWebPage *bool  `json:"web_page,omitempty"`
	Format        string `json:"format,omitempty"`
	CreatedAt     int64  `json:"created_at,omitempty"`
}

// html 判断这条要不要按 HTML 发。
func (e entry) html() bool { return e.Format == "html" || e.LegacyWebPage != nil }

// plain 是这条去掉格式后的文字，列表预览用。按 HTML 存的解析一遍；解析不了就用原文。
func (e entry) plain() string {
	if e.html() {
		if text, _, err := bot.ParseHTML(e.Text); err == nil {
			return text
		}
	}
	return e.Text
}

type document struct {
	Affs []entry `json:"affs"`
	// Aff 是 TeleBox 原版只存一条时的字段，读到了就并进 Affs。
	Aff *entry `json:"aff,omitempty"`
}

func defaults() document { return document{Affs: []entry{}} }

var (
	linkPattern = regexp.MustCompile(`https?://[^\s]+`)
	// indexPattern 和 MiBox 一样认可带负号的数字，负数和 0 报「没有这一条」，不当成子命令。
	indexPattern = regexp.MustCompile(`^-?\d+$`)
)

func help(prefix string) string {
	p := command.Escape(prefix)
	c := func(text string) string { return "<code>" + p + "aff" + text + "</code>" }
	return "✈️ <b>机场 Aff 信息</b>\n\n在别人打算买机场的时候光速发出自己的 Aff 信息，可以存多条。\n\n" +
		"• " + c("") + " 发送默认的一条；存了多条时显示列表\n" +
		"• " + c(" 序号") + " 发送指定的一条\n" +
		"• " + c(" save") + " 回复一条消息，把它存为新的一条，格式和链接都保留\n" +
		"• " + c(" list [页码]") + " 查看列表，每页 " + strconv.Itoa(pageSize) + " 条\n" +
		"• " + c(" remove 序号") + " 删除指定的一条\n\n" +
		"最多存 " + strconv.Itoa(maxEntries) + " 条，每条最多 " + strconv.Itoa(maxLength) + " 字，序号从 1 开始。"
}

// Register 注册 .aff。数据存在 data/aff.json。
func Register(a *app.App) {
	data := kit.NewStore(a, "aff.json", defaults)
	a.Registry.Register(&command.Command{Name: "aff", Group: command.GroupMedia, Description: "管理并发送 Aff 信息",
		Usage: "[序号|save|list [页码]|remove 序号]", Help: help,
		Handle: func(ctx context.Context, inv *command.Invocation) error { return handle(ctx, inv, data) }})
}

func handle(ctx context.Context, inv *command.Invocation, data *store.Store[document]) error {
	doc, err := load(data)
	if err != nil {
		return err
	}
	switch sub := strings.ToLower(inv.Arg(0)); {
	case sub == "":
		switch len(doc.Affs) {
		case 0:
			return kit.Failf("还没有保存 Aff，回复一条消息发 %saff save 保存", inv.Prefix)
		case 1:
			return send(ctx, inv, doc.Affs[0])
		}
		return list(ctx, inv, doc.Affs, "1")
	case sub == "list" || sub == "ls":
		return list(ctx, inv, doc.Affs, kit.OrDefault(inv.Arg(1), "1"))
	case sub == "save":
		return save(ctx, inv, data)
	case sub == "remove" || sub == "rm" || sub == "del":
		return remove(ctx, inv, data)
	case indexPattern.MatchString(sub):
		index, err := strconv.Atoi(sub)
		if err != nil || index < 1 || index > len(doc.Affs) {
			return kit.Failf("没有第 %s 条，%saff list 查看全部", sub, inv.Prefix)
		}
		return send(ctx, inv, doc.Affs[index-1])
	}
	return kit.Failf("没有这个子命令：%s，%shelp aff 查看用法", inv.Arg(0), inv.Prefix)
}

// load 读出文档；还留着 TeleBox 原版的单条字段时，先把它并进列表存回去。
func load(data *store.Store[document]) (document, error) {
	doc, err := data.Read()
	if err != nil || doc.Aff == nil {
		return doc, err
	}
	if err := data.Update(func(doc *document) error {
		if doc.Aff != nil {
			doc.Affs = append(doc.Affs, *doc.Aff)
			doc.Aff = nil
		}
		return nil
	}); err != nil {
		return doc, err
	}
	return data.Read()
}

// send 把命令消息改成这条的内容。按 HTML 存的解析不了（比如 MiBox v2 把含 < 的纯文本
// 标成了 HTML），就退回原样发送，不至于整条发不出去。
func send(ctx context.Context, inv *command.Invocation, item entry) error {
	if item.html() {
		if _, _, err := bot.ParseHTML(item.Text); err == nil {
			return inv.Edit(ctx, item.Text)
		}
	}
	return inv.EditText(ctx, item.Text)
}

// pages 是 count 条分成的页数，至少 1 页。
func pages(count int) int { return max(1, (count+pageSize-1)/pageSize) }

// parsePage 校验页码：正整数，不超过总页数。
func parsePage(raw string, count int) (int, bool) {
	if !kit.IsDigits(raw) || strings.HasPrefix(raw, "0") {
		return 0, false
	}
	page, err := strconv.Atoi(raw)
	if err != nil || page > pages(count) {
		return 0, false
	}
	return page, true
}

// preview 是列表里的一行：空白合并成一个空格，最多 previewRunes 个字。
func preview(item entry) string {
	return command.Truncate(strings.Join(strings.Fields(item.plain()), " "), previewRunes)
}

// renderList 是第 page 页的列表。
func renderList(prefix string, items []entry, page int) string {
	total := pages(len(items))
	var rows []string
	for index := (page - 1) * pageSize; index < min(page*pageSize, len(items)); index++ {
		rows = append(rows, strconv.Itoa(index+1)+". "+command.Escape(preview(items[index])))
	}
	text := "✈️ <b>Aff 列表</b> · " + strconv.Itoa(page) + "/" + strconv.Itoa(total) + "\n\n" + strings.Join(rows, "\n") +
		"\n\n" + command.Code(prefix+"aff 序号") + " 发送指定的一条"
	if page < total {
		text += "，" + command.Code(prefix+"aff list "+strconv.Itoa(page+1)) + " 看下一页"
	}
	return text
}

func list(ctx context.Context, inv *command.Invocation, items []entry, raw string) error {
	if len(items) == 0 {
		return inv.Edit(ctx, "✈️ <b>Aff 列表</b>\n\n还没有保存任何一条，回复一条消息发 "+command.Code(inv.Prefix+"aff save")+" 保存")
	}
	page, ok := parsePage(raw, len(items))
	if !ok {
		return kit.Failf("页码无效，共 %d 页", pages(len(items)))
	}
	return inv.Edit(ctx, renderList(inv.Prefix, items, page))
}

// newEntry 把被回复的消息做成一条：正文连同格式存成 HTML，标上 format，和 MiBox v2 后期的一样。
func newEntry(message *tg.Message, text string, now time.Time) entry {
	var entities []tg.MessageEntityClass
	if message != nil {
		entities = message.Entities
	}
	content := entitiesHTML(text, entities)
	hasLink := linkPattern.MatchString(content)
	return entry{Text: content, WebPage: &hasLink, Format: "html", CreatedAt: now.UnixMilli()}
}

func save(ctx context.Context, inv *command.Invocation, data *store.Store[document]) error {
	reply, err := kit.Reply(ctx, inv)
	if err != nil {
		return err
	}
	if reply == nil {
		return kit.Failf("请回复要保存的消息再发 %saff save", inv.Prefix)
	}
	if strings.TrimSpace(reply.Text) == "" {
		return kit.Fail("被回复的消息没有文字")
	}
	if kit.UTF16Len(reply.Text) > maxLength {
		return kit.Failf("文字超过 %d 字，没有保存，缩短后再试", maxLength)
	}
	index, err := appendEntry(data, newEntry(reply.Raw, reply.Text, time.Now()))
	if err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ 已保存为第 "+strconv.Itoa(index)+" 条")
}

// appendEntry 存一条，返回它的序号。满了就不存。
func appendEntry(data *store.Store[document], item entry) (int, error) {
	index := 0
	err := data.Update(func(doc *document) error {
		if len(doc.Affs) >= maxEntries {
			return kit.Failf("已经存了 %d 条，先删掉不需要的再保存", maxEntries)
		}
		doc.Affs = append(doc.Affs, item)
		index = len(doc.Affs)
		return nil
	})
	return index, err
}

func remove(ctx context.Context, inv *command.Invocation, data *store.Store[document]) error {
	raw := inv.Arg(1)
	if raw == "" {
		return kit.Usage(inv.Prefix, "aff remove 序号")
	}
	index, err := strconv.Atoi(raw)
	if !kit.IsDigits(raw) || err != nil || index < 1 {
		return kit.Fail("序号要写正整数")
	}
	if err := removeEntry(data, index); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ 已删除第 "+strconv.Itoa(index)+" 条")
}

// removeEntry 删掉第 index 条（从 1 数），后面的依次前移。
func removeEntry(data *store.Store[document], index int) error {
	return data.Update(func(doc *document) error {
		if index < 1 || index > len(doc.Affs) {
			return kit.Failf("没有第 %d 条", index)
		}
		doc.Affs = append(doc.Affs[:index-1], doc.Affs[index:]...)
		return nil
	})
}
