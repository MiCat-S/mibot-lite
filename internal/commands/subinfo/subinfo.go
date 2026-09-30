// Package subinfo 实现 .subinfo 和 .cha：读取代理订阅，统计节点数、协议和地区，
// 从响应头读出流量与到期时间。.cha 是简要版。订阅链接里带着 Token，
// 聊天、TXT 报告和日志里都只出现打过码的链接。
package subinfo

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// maxLinks 是一次最多查的订阅数。每个订阅最多要等 15 秒，官网再等 10 秒，
// 不设上限的话一条长消息就能让主机连着请求好几十分钟。
const maxLinks = 20

// linkPattern 认 http(s) 链接。订阅地址都是 ASCII（非 ASCII 字符会被百分号编码），所以遇到
// 非 ASCII 字符就当链接结束：MiBox 会把紧跟在链接后面的中文标点（「，」「。」）一起算进去。
// 不区分大小写只用在协议上：整个模式不区分大小写时，s 会和 ſ（U+017F）算作同一个字母而被排除。
var linkPattern = regexp.MustCompile(`(?i:https?)://[^\s"'<>\x{80}-\x{10FFFF}]+`)

func help(prefix string) string {
	p := command.Escape(prefix)
	item := func(command, args, text string) string {
		return "• <code>" + p + command + " " + args + "</code> " + text + "\n"
	}
	return "📈 <b>订阅信息查询</b>\n\n查询订阅的节点数量、协议、地区分布、流量与到期时间。\n\n" +
		item("subinfo", "订阅链接…", "查询一个或多个订阅") +
		item("subinfo", "txt 订阅链接…", "结果写成 TXT 文件发送") +
		item("cha", "订阅链接…", "简要查询") +
		item("cha", "txt 订阅链接…", "简要结果写成 TXT 文件发送") +
		"• 回复一条带订阅链接的消息，发送 <code>" + p + "subinfo</code> 或 <code>" + p + "cha</code>\n\n" +
		"<b>支持的内容</b>\n" +
		"• Clash YAML/JSON 的 proxies 列表\n" +
		"• 明文或 Base64 编码的节点链接\n" +
		"• VMess、VLESS、Trojan、SS、SSR、Hysteria、TUIC、WireGuard 等协议\n" +
		"• 流量和到期时间取自服务端返回的 subscription-userinfo 响应头\n" +
		"• 地区按节点名称识别，认不出的归入「其他」\n\n" +
		fmt.Sprintf("一次最多 %d 个链接，只访问公网地址。聊天里最多列出 %d 个节点，结果超过 %d 页时改发 TXT 文件。订阅链接里带着 Token，结果里只显示打过码的链接。", maxLinks, listedNodes, maxPages)
}

type service struct {
	fetch *fetcher
	now   func() time.Time
}

// Register 注册 .subinfo（完整）和 .cha（简要），两者参数相同。
func Register(a *app.App) {
	s := &service{fetch: newFetcher(), now: time.Now}
	register := func(name, description string, concise bool) {
		a.Registry.Register(&command.Command{Name: name, Group: command.GroupTools, Description: description,
			Usage: "[txt] [订阅链接…]", Help: help,
			// 最多 20 个订阅，每个要等订阅和官网，默认的 5 分钟不够。
			Timeout: 15 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error {
				return s.handle(ctx, inv, concise)
			}})
	}
	register("subinfo", "查看订阅的节点与流量", false)
	register("cha", "简要查看订阅信息", true)
}

// links 从文字里挑出 http(s) 链接，去重，太长的（超过 2048 字符）不要。
func links(text string) []string {
	seen := map[string]bool{}
	var list []string
	for _, match := range linkPattern.FindAllString(text, -1) {
		if len(match) <= 2048 && !seen[match] {
			seen[match] = true
			list = append(list, match)
		}
	}
	return list
}

// entityLinks 取出消息里藏在文字链接（text_url）后面的地址：订阅常常以「点此订阅」的形式发出。
func entityLinks(message *bot.Message) string {
	if message == nil || message.Raw == nil {
		return ""
	}
	var list []string
	for _, entity := range message.Raw.Entities {
		if link, ok := entity.(*tg.MessageEntityTextURL); ok {
			list = append(list, link.URL)
		}
	}
	return strings.Join(list, " ")
}

// maskLink 给订阅链接打码：只留协议、域名和最后 4 个字符，够分辨是哪一个，又不露 Token。
func maskLink(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "***"
	}
	rest := strings.TrimPrefix(raw, parsed.Scheme+"://"+parsed.Host)
	if runes := []rune(rest); len(runes) > 8 {
		return parsed.Scheme + "://" + parsed.Host + "/…" + string(runes[len(runes)-4:])
	}
	return parsed.Scheme + "://" + parsed.Host + "/…"
}

// failureText 是取订阅失败时给用户看的原因，不带地址。
func failureText(err error) string {
	switch {
	case errors.Is(err, errPrivateAddress):
		return "只能读取公网地址的订阅"
	case errors.Is(err, errRedirect):
		return "订阅跳转到了别的域名"
	}
	return httpx.Reason(err)
}

// parseError 表示订阅取到了，但内容解析不了；和取不到分开说。
type parseError struct{ text string }

func (e parseError) Error() string { return e.text }

// failureSentence 是一个订阅失败时给用户看的整句话。
func failureSentence(err error) string {
	var parse parseError
	if errors.As(err, &parse) {
		return "订阅解析失败：" + parse.text
	}
	if text, ok := kit.IsUserError(err); ok {
		return text
	}
	return "订阅读取失败：" + failureText(err)
}

func (s *service) handle(ctx context.Context, inv *command.Invocation, concise bool) error {
	args := inv.Args
	txt := len(args) > 0 && strings.EqualFold(args[0], "txt")
	if txt {
		args = args[1:]
	}
	source := strings.Join(args, " ")
	if inv.Message.ReplyToID != 0 || len(args) == 0 {
		reply, err := kit.Reply(ctx, inv)
		switch {
		case err != nil && len(links(source)) == 0:
			return err
		case err != nil:
			// 自己已经给了链接，读不到被回复的消息只是少几个链接。
			inv.Log.Warn("subinfo.reply_unavailable", "error", err.Error())
		case reply != nil:
			source += " " + reply.Text + " " + entityLinks(reply)
		}
	}
	list := links(source)
	if len(list) == 0 {
		return inv.EditPages(ctx, command.HTMLPages(help(inv.Prefix), command.PageLimit))
	}
	if len(list) > maxLinks {
		return kit.Failf("一次最多查询 %d 个订阅，这次有 %d 个", maxLinks, len(list))
	}
	if err := inv.EditText(ctx, kit.Working(fmt.Sprintf("正在读取 %d 个订阅", len(list)))); err != nil {
		return err
	}
	names := s.fetch.mappings(ctx)
	var results []outcome
	for _, link := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		report, err := s.inspect(ctx, link, names)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			inv.Log.Info("subinfo.failed", "link", maskLink(link), "error", failureSentence(err))
			if len(list) == 1 {
				return kit.Fail(failureSentence(err))
			}
		}
		results = append(results, outcome{link: link, report: report, err: err})
	}
	if txt {
		return s.sendText(ctx, inv, render(results, concise, true), len(list), false)
	}
	var pages []string
	for _, document := range render(results, concise, false) {
		pages = append(pages, command.HTMLPages(document, command.PageLimit)...)
	}
	// 批量查询、节点很多时结果可能有十几页；超过 maxPages 页就改发 TXT 报告，不刷屏。
	if len(pages) > maxPages {
		return s.sendText(ctx, inv, render(results, concise, true), len(list), true)
	}
	return inv.EditPages(ctx, pages)
}

// outcome 是一个订阅的查询结果：report 和 err 只有一个不为空。
type outcome struct {
	link   string
	report *report
	err    error
}

// render 把各个订阅的结果写成 HTML。full 为真时写 TXT 报告用的完整版，列出全部节点和协议。
func render(results []outcome, concise, full bool) []string {
	documents := make([]string, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			documents = append(documents, failedDocument(result.link, result.err))
			continue
		}
		documents = append(documents, renderReport(result.report, concise, len(results) > 1, full))
	}
	return documents
}

// report 是一个订阅查出来的全部内容。
type report struct {
	link, name, website, traffic string
	summary                      summary
}

// inspect 同时取订阅和官网信息，再解析订阅内容。
func (s *service) inspect(ctx context.Context, link string, names []mapping) (*report, error) {
	parsed, err := url.Parse(link)
	if err != nil || parsed.Host == "" {
		return nil, kit.Fail("链接无效")
	}
	var info site
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		info = s.fetch.siteInfo(ctx, parsed)
	}()
	fetched, err := s.fetch.fetchSubscription(ctx, parsed)
	wait.Wait()
	if err != nil {
		return nil, err
	}
	content, err := parseSubscription(fetched.text)
	if err != nil {
		text, _ := kit.IsUserError(err)
		return nil, parseError{text: text}
	}
	// 机场名：先查对照表，再看响应头里的文件名，最后用官网标题。
	name := mappedName(link, names)
	if name == "" {
		name = headerName(fetched.contentDisposition)
	}
	if name == "" {
		name = kit.OrDefault(info.name, "未知")
	}
	// 名字来自对方服务器的响应头或网页标题，长度不受控制，截到 nameLimit 个字。
	name = command.Truncate(name, nameLimit)
	// 官网：响应头给了个人中心地址就用它的域名部分（完整地址里可能带登录凭据），否则用订阅的域名。
	website := info.website
	if profile, err := url.Parse(fetched.profileURL); err == nil && profile.Host != "" && (profile.Scheme == "https" || profile.Scheme == "http") {
		website = profile.Scheme + "://" + profile.Host
	}
	return &report{link: maskLink(link), name: name, website: website,
		traffic: trafficSummary(fetched.userinfo, fetched.hasUserinfo, s.now()), summary: content}, nil
}

// 订阅内容由对方的服务器决定：2 MB 的订阅能有十几万个节点、名字也可以很长，全列出来就是
// 几百条消息，刷屏还会被限流。聊天里只列一部分，完整列表在 TXT 报告里。
const (
	// listedNodes 是聊天里最多列出的节点数。
	listedNodes = 200
	// listedProtocols 是聊天里最多列出的协议种数：Clash 配置里的协议名可以随便写。
	listedProtocols = 20
	// nameLimit 是机场名、节点名最多保留的字数。
	nameLimit = 64
	// maxPages 是聊天里最多发的页数，超过就改发 TXT 报告。
	maxPages = 5
)

// renderReport 写出一个订阅的结果。简要版不列协议和地区分布，但带打码的订阅链接；
// 完整版在批量查询时也带上链接，好分清是哪一个。两者都附节点列表。full 为假时
// 节点和协议只列前面一部分，末尾注明总数。
func renderReport(r *report, concise, batch, full bool) string {
	var b strings.Builder
	b.WriteString("📈 <b>订阅信息</b>\n")
	b.WriteString("<b>机场名称</b>：" + command.Code(r.name) + "\n")
	b.WriteString("<b>官网链接</b>：" + command.Escape(r.website) + "\n")
	if concise || batch {
		b.WriteString("<b>订阅链接</b>：" + command.Code(r.link) + "\n")
	}
	b.WriteString(fmt.Sprintf("<b>节点总数</b>：%d\n\n", len(r.summary.names)))
	b.WriteString("<b>流量与到期</b>\n" + command.Escape(r.traffic))
	if !concise {
		lines := r.summary.protocols.lines()
		if !full && len(lines) > listedProtocols {
			lines = append(lines[:listedProtocols:listedProtocols], fmt.Sprintf("…还有 %d 种", len(lines)-listedProtocols))
		}
		protocols := strings.Join(lines, "\n")
		if protocols == "" {
			protocols = "未识别常见节点协议"
		}
		b.WriteString("\n\n<b>协议分布</b>\n<pre>" + command.Escape(protocols) + "</pre>")
		if regions := r.summary.regions.lines(); len(regions) > 0 {
			b.WriteString("\n<b>地区分布</b>\n" + command.Escape(strings.Join(regions, " · ")))
		}
	}
	if total := len(r.summary.names); total > 0 {
		shown := total
		if !full {
			shown = min(total, listedNodes)
		}
		lines := make([]string, 0, shown+1)
		for index, name := range r.summary.names[:shown] {
			lines = append(lines, fmt.Sprintf("%d. %s", index+1, command.Truncate(name, nameLimit)))
		}
		if shown < total {
			lines = append(lines, fmt.Sprintf("…共 %d 个，完整列表用 txt 参数查看", total))
		}
		b.WriteString("\n\n<b>节点列表</b>\n<pre>" + command.Escape(strings.Join(lines, "\n")) + "</pre>")
	}
	return b.String()
}

// failedDocument 是批量查询里失败的那一个。
func failedDocument(link string, err error) string {
	return "📈 <b>订阅信息</b>\n<b>订阅链接</b>：" + command.Code(maskLink(link)) + "\n❌ " + command.Escape(failureSentence(err))
}

var tagPattern = regexp.MustCompile(`<[^>]+>`)

// plainText 把结果的 HTML 变回纯文本，写进 TXT 报告。
func plainText(documents []string) string {
	return html.UnescapeString(tagPattern.ReplaceAllString(strings.Join(documents, "\n\n"), "")) + "\n"
}

// sendText 把结果写成 TXT 文件发到当前对话，然后删掉命令消息，和 MiBox 一样。
// tooLong 为真表示用户没要 TXT，是结果太长才改发文件，说明里讲清楚。
func (s *service) sendText(ctx context.Context, inv *command.Invocation, documents []string, count int, tooLong bool) error {
	peer, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	name := "subinfo-" + s.now().Format("20060102-1504") + ".txt"
	caption := fmt.Sprintf("✅ 已生成订阅报告（共 %d 个链接）", count)
	if tooLong {
		caption = fmt.Sprintf("✅ 结果太长，已生成订阅报告（共 %d 个链接）", count)
	}
	if err := inv.Client.SendDocument(ctx, peer, []byte(plainText(documents)),
		bot.MediaOptions{Name: name, MimeType: "text/plain", Caption: caption, ReplyTo: inv.Message.ReplyToID, ForceDocument: true}); err != nil {
		return kit.FailWith("发送报告失败", err)
	}
	if err := inv.Client.DeleteMessage(ctx, inv.Message); err != nil {
		inv.Log.Warn("subinfo.cleanup_failed", "error", err.Error())
	}
	return nil
}
