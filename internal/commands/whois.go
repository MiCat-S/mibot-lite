package commands

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

type whoisItem struct {
	Domain    string `json:"domain"`
	RawData   string `json:"rawData"`
	QueryTime string `json:"queryTime"`
}

type whoisData struct {
	History []whoisItem          `json:"history"`
	Cache   map[string]whoisItem `json:"cache"`
}

var (
	// 域名的每一段都可以含数字和连字符，只要求最后一段是字母。MiBox 的
	// 正则要求第一段之后的每一段都只能是字母、而且至少两个，所以
	// a.b.co.uk（第二段只有一个字母）和 mail.1and1.com（中间段带数字）
	// 这样的真实域名都被拒掉了。
	domainPattern    = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,}$`)
	loosePattern     = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?[a-z0-9][a-z0-9.-]*\.[a-z]{2,}`)
	nameServerRegexp = regexp.MustCompile(`(?im)^(?:Name Server|nserver|NS):[ \t]*(.+)$`)
)

func normalizeDomain(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	value = regexp.MustCompile(`(?i)^https?://`).ReplaceAllString(value, "")
	value = regexp.MustCompile(`(?i)^www\.`).ReplaceAllString(value, "")
	if index := strings.IndexByte(value, '/'); index >= 0 {
		value = value[:index]
	}
	value = strings.ToLower(value)
	if len(value) > 253 || !domainPattern.MatchString(value) {
		return "", false
	}
	return value, true
}

func extractWhois(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Data struct {
				Whois struct {
					Whois string `json:"whois"`
				} `json:"whois"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &event); err == nil && event.Data.Whois.Whois != "" {
			return event.Data.Whois.Whois
		}
	}
	return ""
}

func whoisField(raw, name string) string {
	match := regexp.MustCompile(`(?im)^` + regexp.QuoteMeta(name) + `:[ \t]*(.*)$`).FindStringSubmatch(raw)
	if len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func whoisReport(domain, raw string) []string {
	expiry := whoisField(raw, "Registry Expiry Date")
	if expiry == "" {
		expiry = whoisField(raw, "Registrar Registration Expiration Date")
	}
	lines := []string{"WHOIS 结果\n" + domain}
	for _, field := range [][2]string{
		{"注册商", whoisField(raw, "Registrar")}, {"注册日期", whoisField(raw, "Creation Date")},
		{"更新日期", whoisField(raw, "Updated Date")}, {"到期日期", expiry}, {"域名状态", whoisField(raw, "Domain Status")},
	} {
		if field[1] != "" {
			lines = append(lines, field[0]+": "+field[1])
		}
	}
	if expiry != "" {
		if when, err := time.Parse(time.RFC3339, strings.TrimSpace(expiry)); err == nil {
			days := int(time.Until(when).Hours() / 24)
			if days < 0 {
				lines = append(lines, "到期提醒: 已过期")
			} else if days < 90 {
				lines = append(lines, "到期提醒: "+strings.TrimSpace(itoa(days))+" 天后过期")
			}
		}
	}
	servers := map[string]bool{}
	var ordered []string
	for _, match := range nameServerRegexp.FindAllStringSubmatch(raw, -1) {
		value := strings.TrimSpace(match[1])
		if !servers[value] {
			servers[value] = true
			ordered = append(ordered, value)
		}
	}
	if len(ordered) > 0 {
		lines = append(lines, "DNS 服务器:\n"+strings.Join(ordered, "\n"))
	}
	var pages []string
	for _, page := range command.EscapedPages(strings.Join(lines, "\n"), 3400) {
		pages = append(pages, "<pre>"+page+"</pre>")
	}
	for _, page := range command.EscapedPages(raw, 3400) {
		pages = append(pages, "<blockquote expandable>"+page+"</blockquote>")
	}
	return pages
}

func itoa(value int) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune { return r }, formatInt(value)))
}

func formatInt(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// Whois 注册 .whois。
func Whois(a *app.App) {
	data := newStore(a, "whois.json", func() whoisData { return whoisData{Cache: map[string]whoisItem{}} })
	help := func(prefix string) string {
		return "🔍 <b>WHOIS 域名查询</b>\n\n• " + command.Code(prefix+"whois example.com") + "\n• " + command.Code(prefix+"whois") + " 回复含域名的消息\n• " +
			command.Code(prefix+"whois history") + " 查看历史\n• " + command.Code(prefix+"whois clear") + " 清除历史\n查询结果缓存 24 小时。"
	}
	a.Registry.Register(&command.Command{Name: "whois", Description: "查询域名注册信息", Usage: "域名", Help: help, Timeout: 40 * time.Second,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			raw := inv.Arg(0)
			lower := strings.ToLower(raw)
			if lower == "help" || lower == "h" {
				return inv.Edit(ctx, help(inv.Prefix))
			}
			if raw == "" {
				if reply, err := inv.Client.GetReply(ctx, inv.Message); err == nil && reply != nil {
					raw = loosePattern.FindString(reply.Text)
				}
				if raw == "" {
					return inv.Edit(ctx, help(inv.Prefix))
				}
			}
			switch lower {
			case "clear":
				var counts [2]int
				_ = data.Update(func(d *whoisData) error {
					counts = [2]int{len(d.History), len(d.Cache)}
					d.History = nil
					d.Cache = map[string]whoisItem{}
					return nil
				})
				return inv.EditText(ctx, "已清除历史 "+itoa(counts[0])+" 条、缓存 "+itoa(counts[1])+" 个域名")
			case "history":
				current, _ := data.Read()
				var rows []string
				for index, item := range current.History {
					if index >= 20 {
						break
					}
					stamp := item.QueryTime
					if len(stamp) >= 16 {
						stamp = strings.Replace(stamp[5:16], "T", " ", 1)
					}
					rows = append(rows, itoa(index+1)+". "+command.Code(item.Domain)+" <i>"+command.Escape(stamp)+"</i>")
				}
				body := "暂无查询历史"
				if len(rows) > 0 {
					body = strings.Join(rows, "\n")
				}
				return inv.Edit(ctx, "<b>WHOIS 查询历史</b>\n\n"+body+"\n\n共 "+itoa(len(current.History))+" 条，缓存 "+itoa(len(current.Cache))+" 个")
			}
			name, ok := normalizeDomain(raw)
			if !ok {
				return inv.Edit(ctx, "请输入有效域名，例如 "+command.Code("example.com"))
			}
			if err := inv.Edit(ctx, "🔍 正在查询 "+command.Code(name)+"…"); err != nil {
				return err
			}
			result, err := whoisQuery(ctx, name, data)
			if err != nil {
				return inv.EditText(ctx, "WHOIS 查询失败，请稍后重试")
			}
			if result == "" {
				return inv.EditText(ctx, "未取得 WHOIS 数据")
			}
			return sendPages(ctx, inv, whoisReport(name, result))
		}})
}

func whoisQuery(ctx context.Context, name string, data *store.Store[whoisData]) (string, error) {
	current, err := data.Read()
	if err != nil {
		return "", err
	}
	if cached, ok := current.Cache[name]; ok {
		if when, err := time.Parse(time.RFC3339, cached.QueryTime); err == nil && time.Since(when) < 24*time.Hour {
			return cached.RawData, nil
		}
	}
	response, err := httpx.Do(ctx, httpx.Request{URL: "https://namebeta.com/api/search/check?query=" + url.QueryEscape(name),
		Headers: map[string]string{"User-Agent": "Mi Box"}, Timeout: 15 * time.Second, MaxBytes: 4 << 20})
	if err != nil || !response.OK() {
		return "", err
	}
	result := extractWhois(string(response.Body))
	if result == "" {
		return "", nil
	}
	item := whoisItem{Domain: name, RawData: result, QueryTime: time.Now().UTC().Format(time.RFC3339)}
	_ = data.Update(func(d *whoisData) error {
		history := append([]whoisItem{item}, d.History...)
		if len(history) > 100 {
			history = history[:100]
		}
		d.History = history
		if d.Cache == nil {
			d.Cache = map[string]whoisItem{}
		}
		d.Cache[name] = item
		return nil
	})
	return result, nil
}
