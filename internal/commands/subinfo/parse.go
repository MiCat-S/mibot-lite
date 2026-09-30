package subinfo

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"math/bits"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-faster/yaml"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/sysinfo"
)

// protocols 是认得出的节点链接协议，按 MiBox 的顺序逐个比对「协议://」前缀。
var protocols = []string{"vmess", "vless", "trojan", "ss", "ssr", "hysteria2", "hy2", "tuic", "socks", "socks5",
	"hysteria", "hy", "wireguard", "http", "https", "shadowtls", "naive"}

// region 是一个地区和用来从节点名里认出它的关键词。纯字母的关键词要作为独立的词出现
// （hk 不能匹配 thkx 里的 hk），其余的只要包含就算。
type region struct {
	name  string
	words []string
}

var regions = []region{
	{"香港", []string{"香港", "hong kong", "hongkong", "hkg", "hk"}},
	{"台湾", []string{"台湾", "taiwan", "taipei", "tpe", "tw"}},
	{"日本", []string{"日本", "japan", "osaka", "jap", "jp", "tokyo"}},
	{"新加坡", []string{"新加坡", "singapore", "sgp", "sg"}},
	{"韩国", []string{"韩国", "korea", "seoul", "kor", "kr"}},
	{"印度", []string{"印度", "india"}},
	{"马来西亚", []string{"马来西亚", "malaysia"}},
	{"泰国", []string{"泰国", "thailand"}},
	{"越南", []string{"越南", "vietnam"}},
	{"印度尼西亚", []string{"印度尼西亚", "indonesia"}},
	{"菲律宾", []string{"菲律宾", "philippines"}},
	{"土耳其", []string{"土耳其", "turkey"}},
	{"美国", []string{"美国", "united states", "usa", "us"}},
	{"加拿大", []string{"加拿大", "canada", "ca"}},
	{"英国", []string{"英国", "united kingdom", "uk", "london"}},
	{"德国", []string{"德国", "germany", "de"}},
	{"法国", []string{"法国", "france"}},
	{"荷兰", []string{"荷兰", "netherlands"}},
	{"瑞士", []string{"瑞士", "switzerland"}},
	{"意大利", []string{"意大利", "italy"}},
	{"西班牙", []string{"西班牙", "spain"}},
	{"澳大利亚", []string{"澳大利亚", "australia", "au"}},
	{"新西兰", []string{"新西兰", "new zealand"}},
	{"巴西", []string{"巴西", "brazil"}},
	{"阿联酋", []string{"阿联酋", "uae"}},
	{"以色列", []string{"以色列", "israel"}},
	{"南非", []string{"南非", "south africa"}},
	{"俄罗斯", []string{"俄罗斯", "russia"}},
}

var (
	lettersOnly = regexp.MustCompile(`^[a-z]+$`)
	// wordPatterns 是纯字母关键词对应的「前后不是字母」匹配，启动时编好。
	wordPatterns = func() map[string]*regexp.Regexp {
		patterns := map[string]*regexp.Regexp{}
		for _, item := range regions {
			for _, word := range item.words {
				if lettersOnly.MatchString(word) {
					patterns[word] = regexp.MustCompile(`(?:^|[^a-z])` + word + `(?:$|[^a-z])`)
				}
			}
		}
		return patterns
	}()
	clashStart = regexp.MustCompile(`(?m)^(?:proxies[\s\p{Z}\x{FEFF}]*:|\{)`)
	clashType  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// regionOf 按节点名认地区，认不出返回 ""。
func regionOf(name string) string {
	lower := strings.ToLower(name)
	for _, item := range regions {
		for _, word := range item.words {
			if pattern, ok := wordPatterns[word]; ok {
				if pattern.MatchString(lower) {
					return item.name
				}
			} else if strings.Contains(lower, word) {
				return item.name
			}
		}
	}
	return ""
}

// tally 是按出现顺序排的计数（协议、地区），显示时照这个顺序。
type tally struct {
	names  []string
	counts map[string]int
}

func (t *tally) add(name string) { t.addN(name, 1) }

func (t *tally) addN(name string, n int) {
	if t.counts == nil {
		t.counts = map[string]int{}
	}
	if t.counts[name] == 0 {
		t.names = append(t.names, name)
	}
	t.counts[name] += n
}

// lines 把计数写成「名字：数量」。
func (t *tally) lines() []string {
	lines := make([]string, 0, len(t.names))
	for _, name := range t.names {
		lines = append(lines, name+"："+strconv.Itoa(t.counts[name]))
	}
	return lines
}

// summary 是一份订阅解析出来的内容：只有节点名和协议，不留服务器、密码这些凭据。
type summary struct {
	names     []string
	protocols tally
	regions   tally
}

// decodeBase64 宽松地解 Base64，和 Node 的 Buffer.from(…, "base64") 一样：忽略空白，
// 标准和 URL 安全两种字母表都认，末尾的 = 可有可无。
func decodeBase64(text string) (string, bool) {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
	cleaned = strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimRight(cleaned, "="))
	if len(cleaned)%4 == 1 {
		cleaned = cleaned[:len(cleaned)-1]
	}
	decoded, err := base64.RawStdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", false
	}
	return strings.ToValidUTF8(string(decoded), "�"), true
}

// parseSubscription 解析订阅内容：先试整段 Base64（解出来含「://」才算），再看是 Clash 的
// proxies 列表（YAML 或 JSON）还是一行一个的节点链接。
func parseSubscription(raw string) (summary, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return summary{}, kit.Fail("订阅内容为空")
	}
	if decoded, ok := decodeBase64(text); ok && strings.Contains(decoded, "://") {
		text = decoded
	}
	var result summary
	add := func(kind, name string) {
		result.names = append(result.names, name)
		result.protocols.add(kind)
	}
	if clashStart.MatchString(text) {
		if err := parseClash(text, add); err != nil {
			return summary{}, err
		}
	} else {
		count := 0
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			lower := strings.ToLower(line)
			for _, kind := range protocols {
				if strings.HasPrefix(lower, kind+"://") {
					add(kind, nodeName(line, kind, count))
					count++
					break
				}
			}
		}
	}
	other := 0
	for _, name := range result.names {
		if found := regionOf(name); found != "" {
			result.regions.add(found)
		} else {
			other++
		}
	}
	if other > 0 {
		result.regions.addN("其他", other)
	}
	return result, nil
}

// parseClash 读 Clash 配置里的 proxies：每项必须有协议类型，名字没有就写「协议 序号」。
// 只取名字和协议，节点对象里的服务器、密码一概不碰。JSON 也是合法的 YAML，一起交给 YAML 解析，
// 与 MiBox 用 js-yaml 读两种格式一样。
func parseClash(text string, add func(kind, name string)) error {
	var document map[string]any
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		return kit.Fail("订阅不是有效的 Clash 配置")
	}
	proxies, ok := document["proxies"].([]any)
	if !ok {
		return kit.Fail("无效的 Clash 节点列表")
	}
	for index, item := range proxies {
		proxy, ok := item.(map[string]any)
		if !ok {
			return kit.Fail("无效的 Clash 节点")
		}
		kind, ok := proxy["type"].(string)
		if !ok || strings.TrimSpace(kind) == "" {
			return kit.Fail("无效的 Clash 节点")
		}
		kind = strings.ToLower(kind)
		if !clashType.MatchString(kind) {
			return kit.Fail("无效的 Clash 协议")
		}
		name, ok := proxy["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			name = strings.ToUpper(kind) + " " + strconv.Itoa(index+1)
		}
		add(kind, name)
	}
	return nil
}

// nodeName 取节点链接的名字：先看 # 后面的部分，VMess 看 Base64 JSON 里的 ps，
// SSR 看参数里 Base64 编码的 remarks，都没有就写「协议 序号」。
func nodeName(line, kind string, index int) string {
	if hash := strings.Index(line, "#"); hash >= 0 && hash+1 < len(line) {
		fragment := line[hash+1:]
		if decoded, err := url.PathUnescape(fragment); err == nil {
			return decoded
		}
		return fragment
	}
	body := line[strings.Index(line, "://")+3:]
	switch kind {
	case "vmess":
		if decoded, ok := decodeBase64(body); ok {
			var data map[string]any
			if json.Unmarshal([]byte(decoded), &data) == nil {
				if ps, ok := data["ps"].(string); ok && strings.TrimSpace(ps) != "" {
					return ps
				}
			}
		}
	case "ssr":
		if decoded, ok := decodeBase64(body); ok {
			if query := strings.Index(decoded, "/?"); query >= 0 {
				if values, err := url.ParseQuery(decoded[query+2:]); err == nil {
					if remarks := values.Get("remarks"); remarks != "" {
						if name, ok := decodeBase64(remarks); ok && strings.TrimSpace(name) != "" {
							return name
						}
					}
				}
			}
		}
	}
	return strings.ToUpper(kind) + " " + strconv.Itoa(index+1)
}

// userinfoField 是 subscription-userinfo 响应头里的一项，如 upload=1024。
var userinfoField = regexp.MustCompile(`(?i)^(upload|download|total|expire|starttime)[\s\p{Z}]*=[\s\p{Z}]*(\d{1,20})$`)

// maxExpire 是认为合理的到期时间上限（秒），再大就当无效，与 MiBox 一致。
const maxExpire = 8_640_000_000_000

// trafficSummary 把 subscription-userinfo 响应头写成几行：上传、下载、已用、总量、剩余和到期。
// 没有这个头时 present 为假。
func trafficSummary(header string, present bool, now time.Time) string {
	if !present || header == "" {
		return "流量统计：未提供"
	}
	fields := map[string]uint64{}
	for _, part := range strings.Split(header, ";") {
		match := userinfoField.FindStringSubmatch(strings.TrimSpace(part))
		if match == nil {
			continue
		}
		// 超过 uint64 的数（20 位里的极少数）当作没提供。
		if value, err := strconv.ParseUint(match[2], 10, 64); err == nil {
			fields[strings.ToLower(match[1])] = value
		}
	}
	upload, hasUpload := fields["upload"]
	download, hasDownload := fields["download"]
	total, hasTotal := fields["total"]
	size := func(value uint64, known bool) string {
		if !known {
			return "未知"
		}
		return sysinfo.FormatBytes(value)
	}
	lines := []string{"上传：" + size(upload, hasUpload), "下载：" + size(download, hasDownload)}
	// 两个都接近 uint64 上限时相加会回绕成很小的数，显示成「已用 0 B」又永远不算耗尽；
	// 溢出时按最大值算。
	used, carry := bits.Add64(upload, download, 0)
	if carry != 0 {
		used = math.MaxUint64
	}
	hasUsed := hasUpload && hasDownload
	if hasUsed {
		lines = append(lines, "已用："+sysinfo.FormatBytes(used))
	}
	switch {
	case !hasTotal:
		lines = append(lines, "总量：未知")
	case total == 0:
		lines = append(lines, "总量：未设限")
	default:
		lines = append(lines, "总量："+sysinfo.FormatBytes(total))
	}
	if hasTotal && total > 0 && hasUsed {
		remaining := uint64(0)
		if total > used {
			remaining = total - used
		}
		lines = append(lines, "剩余："+sysinfo.FormatBytes(remaining))
		if used >= total {
			lines = append(lines, "流量状态：已耗尽")
		}
	}
	expire, hasExpire := fields["expire"]
	switch {
	case !hasExpire:
		lines = append(lines, "到期：未提供")
	case expire == 0:
		lines = append(lines, "到期：未设期限")
	case expire <= maxExpire:
		at := time.Unix(int64(expire), 0).UTC()
		line := "到期：" + at.Format("2006-01-02 15:04") + " UTC"
		if !at.After(now) {
			line += "（已过期）"
		}
		lines = append(lines, line)
	default:
		lines = append(lines, "到期：无效时间")
	}
	return strings.Join(lines, "\n")
}
