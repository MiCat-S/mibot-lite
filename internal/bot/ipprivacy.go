package bot

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// IPPolicy 是发出去的消息里 IP 地址的显示方式，字段名照 MiBox v2 的 ip.json。
type IPPolicy struct {
	// Mode 是 "mask"（末尾几段换成 *）或 "hide"（整个换成「[IP已隐藏]」）。
	Mode string `json:"mode"`
	IPv4 int    `json:"ipv4Segments"`
	IPv6 int    `json:"ipv6Segments"`
}

// DefaultIPPolicy 和 MiBox v2 的默认值一样：IPv4 遮后 2 段，IPv6 遮后 4 段。
var DefaultIPPolicy = IPPolicy{Mode: "mask", IPv4: 2, IPv6: 4}

var ipPolicy atomic.Pointer[IPPolicy]

// Validate 检查设置是否在允许的范围内。
func (p IPPolicy) Validate() error {
	if (p.Mode != "mask" && p.Mode != "hide") || p.IPv4 < 1 || p.IPv4 > 4 || p.IPv6 < 1 || p.IPv6 > 8 {
		return errors.New("IP 显示设置无效：IPv4 1–4 段，IPv6 1–8 段")
	}
	return nil
}

// SetIPPolicy 换掉当前的 IP 显示方式。
func SetIPPolicy(policy IPPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	ipPolicy.Store(&policy)
	return nil
}

// CurrentIPPolicy 是当前的 IP 显示方式，没设过就是 DefaultIPPolicy。
func CurrentIPPolicy() IPPolicy {
	if policy := ipPolicy.Load(); policy != nil {
		return *policy
	}
	return DefaultIPPolicy
}

type ipPrivacyOff struct{}

// WithoutIPPrivacy 标记这次发送不打码。用于替别人代发的命令（.sudo、.sure）和线上自检
// 发的命令：那里的 IP 是命令参数，打了码命令就坏了，而且它本来就是对方自己公开打出来的。
func WithoutIPPrivacy(ctx context.Context) context.Context {
	return context.WithValue(ctx, ipPrivacyOff{}, true)
}

var (
	// ipv6Candidate 和 ipv4Candidate 是 MiBox 的两个正则去掉前后断言的部分；
	// Go 的正则不支持断言，前后字符在 ipReplacements 里另外检查。
	ipv6Candidate = regexp.MustCompile(`(?i)(?:[a-f\d]{0,4}:){2,}[a-f\d:.]*(?:%[\w.-]+)?`)
	ipv4Candidate = regexp.MustCompile(`(?:\d{1,3}\.){3}\d{1,3}`)
)

// ipReplacement 是正文里要换掉的一段，start、end 是字节偏移。
type ipReplacement struct {
	start, end int
	value      string
}

func wordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ipReplacements 找出正文里的 IP 地址和替换成的文字。先找 IPv6 再找 IPv4，重叠的只算一次，
// 规则和 MiBox v2 的 ip-privacy 一致：地址前后不能紧挨着字母、数字或同类分隔符。
func ipReplacements(text string, policy IPPolicy) []ipReplacement {
	var found []ipReplacement
	overlaps := func(start, end int) bool {
		for _, item := range found {
			if start < item.end && end > item.start {
				return true
			}
		}
		return false
	}
	scan := func(pattern *regexp.Regexp, before func(byte) bool, after func(string) bool, version int) {
		for position := 0; position < len(text); {
			location := pattern.FindStringIndex(text[position:])
			if location == nil {
				return
			}
			start, end := position+location[0], position+location[1]
			address := strings.TrimRight(text[start:end], ".")
			end = start + len(address)
			valid := address != "" && (start == 0 || !before(text[start-1])) && after(text[end:])
			if valid {
				parsed, err := netip.ParseAddr(address)
				valid = err == nil && ((version == 4 && parsed.Is4()) || (version == 6 && parsed.Is6()))
			}
			if !valid || overlaps(start, end) {
				// 不算数时从下一个字符接着找，别跳过藏在这一段里面的地址。
				_, size := utf8.DecodeRuneInString(text[start:])
				position = start + max(size, 1)
				continue
			}
			found = append(found, ipReplacement{start: start, end: end, value: maskedAddress(address, version, policy)})
			position = end
		}
	}
	scan(ipv6Candidate, func(b byte) bool { return wordByte(b) || b == ':' },
		func(rest string) bool { return rest == "" || !(wordByte(rest[0]) || rest[0] == ':') }, 6)
	scan(ipv4Candidate, func(b byte) bool { return wordByte(b) || b == '.' },
		func(rest string) bool {
			return rest == "" || !(wordByte(rest[0]) || (rest[0] == '.' && len(rest) > 1 && rest[1] >= '0' && rest[1] <= '9'))
		}, 4)
	for index := 1; index < len(found); index++ {
		for back := index; back > 0 && found[back].start < found[back-1].start; back-- {
			found[back], found[back-1] = found[back-1], found[back]
		}
	}
	return found
}

// maskedAddress 把地址换成打码后的样子。
func maskedAddress(address string, version int, policy IPPolicy) string {
	if policy.Mode == "hide" {
		return "[IP已隐藏]"
	}
	parts, separator, count := strings.Split(address, "."), ".", policy.IPv4
	if version == 6 {
		parts, separator, count = ipv6Groups(address), ":", policy.IPv6
	}
	for index := range parts {
		if index >= len(parts)-count {
			parts[index] = "*"
		}
	}
	return strings.Join(parts, separator)
}

// ipv6Groups 把 IPv6 地址展开成 8 组，保留原来每组的写法；末尾嵌的 IPv4 换成两组十六进制，
// 区域标识（%eth0）去掉。和 MiBox 一样。
func ipv6Groups(address string) []string {
	source, _, _ := strings.Cut(address, "%")
	if strings.Contains(source, ".") {
		index := strings.LastIndex(source, ":")
		var bytes [4]int
		for position, part := range strings.Split(source[index+1:], ".") {
			if position < 4 {
				bytes[position], _ = strconv.Atoi(part)
			}
		}
		source = source[:index+1] + strconv.FormatInt(int64(bytes[0]<<8+bytes[1]), 16) + ":" + strconv.FormatInt(int64(bytes[2]<<8+bytes[3]), 16)
	}
	left, right, compressed := strings.Cut(source, "::")
	var head, tail []string
	if left != "" {
		head = strings.Split(left, ":")
	}
	if !compressed {
		return head
	}
	if right != "" {
		tail = strings.Split(right, ":")
	}
	groups := append([]string{}, head...)
	for len(groups)+len(tail) < 8 {
		groups = append(groups, "0")
	}
	return append(groups, tail...)
}

// MaskIPs 按当前设置把正文里的 IP 地址打码。
func MaskIPs(text string) string {
	masked, _ := applyIPReplacements(text, ipReplacements(text, CurrentIPPolicy()))
	return masked
}

func applyIPReplacements(text string, items []ipReplacement) (string, bool) {
	if len(items) == 0 {
		return text, false
	}
	var out strings.Builder
	cursor := 0
	for _, item := range items {
		out.WriteString(text[cursor:item.start])
		out.WriteString(item.value)
		cursor = item.end
	}
	out.WriteString(text[cursor:])
	return out.String(), true
}

// utf16Len 是 Telegram 算实体偏移用的长度单位。
func utf16Len(text string) int { return len(utf16.Encode([]rune(text))) }

// ipLink 判断一个链接会不会带出 IP：解码后含有 IP 地址，或者主机名本身就是 IP。
func ipLink(value string) bool {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		decoded = value
	}
	if len(ipReplacements(decoded, DefaultIPPolicy)) > 0 {
		return true
	}
	parsed, err := url.Parse(decoded)
	if err != nil {
		return false
	}
	_, err = netip.ParseAddr(strings.Trim(parsed.Hostname(), "[]"))
	return err == nil
}

// redactMessage 打码正文，调整格式实体的位置，去掉会带出 IP 的链接实体。
// 返回新的正文、实体，以及是否有改动（有改动时不该再生成链接预览）。
func redactMessage(text string, entities []tg.MessageEntityClass) (string, []tg.MessageEntityClass, bool) {
	items := ipReplacements(text, CurrentIPPolicy())
	masked, changed := applyIPReplacements(text, items)
	type shift struct{ start, end, length int }
	shifts := make([]shift, len(items))
	for index, item := range items {
		shifts[index] = shift{start: utf16Len(text[:item.start]), end: utf16Len(text[:item.end]), length: utf16Len(item.value)}
	}
	position := func(offset int, end bool) int {
		moved := 0
		for _, item := range shifts {
			if offset <= item.start {
				break
			}
			if offset < item.end {
				result := item.start + moved
				if end {
					result += item.length
				}
				return result
			}
			moved += item.length - (item.end - item.start)
		}
		return offset + moved
	}
	units := utf16.Encode([]rune(text))
	var kept []tg.MessageEntityClass
	for _, entity := range entities {
		offset, length := entity.GetOffset(), entity.GetLength()
		switch value := entity.(type) {
		case *tg.MessageEntityTextURL:
			if ipLink(value.URL) {
				changed = true
				continue
			}
		case *tg.MessageEntityURL:
			if offset >= 0 && offset+length <= len(units) && ipLink(string(utf16.Decode(units[offset:offset+length]))) {
				changed = true
				continue
			}
		}
		if len(shifts) == 0 {
			kept = append(kept, entity)
			continue
		}
		start := position(offset, false)
		finish := position(offset+length, true)
		if finish <= start {
			continue
		}
		kept = append(kept, withRange(entity, start, finish-start))
	}
	return masked, kept, changed
}

// withRange 复制一个实体并改掉它的位置，不动调用方手里的那一份。
func withRange(entity tg.MessageEntityClass, offset, length int) tg.MessageEntityClass {
	source := reflect.ValueOf(entity)
	if source.Kind() != reflect.Pointer || source.Elem().Kind() != reflect.Struct {
		return entity
	}
	copied := reflect.New(source.Elem().Type())
	copied.Elem().Set(source.Elem())
	if field := copied.Elem().FieldByName("Offset"); field.CanSet() {
		field.SetInt(int64(offset))
	}
	if field := copied.Elem().FieldByName("Length"); field.CanSet() {
		field.SetInt(int64(length))
	}
	if value, ok := copied.Interface().(tg.MessageEntityClass); ok {
		return value
	}
	return entity
}

// redactMedia 打码上传文件的文件名。
func redactMedia(media tg.InputMediaClass) tg.InputMediaClass {
	document, ok := media.(*tg.InputMediaUploadedDocument)
	if !ok {
		return media
	}
	copied := *document
	copied.Attributes = make([]tg.DocumentAttributeClass, len(document.Attributes))
	for index, attribute := range document.Attributes {
		if name, ok := attribute.(*tg.DocumentAttributeFilename); ok {
			attribute = &tg.DocumentAttributeFilename{FileName: MaskIPs(name.FileName)}
		}
		copied.Attributes[index] = attribute
	}
	return &copied
}

// IPRedactor 是连接中间件：发消息、编辑消息、发媒体、发相册之前，把正文和文件名里的
// IP 地址按当前设置打码，同 MiBox v2 的 installIpPrivacy。命令各自不用管，漏不掉。
type IPRedactor struct{}

// Handle 实现 telegram.Middleware。
func (IPRedactor) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		if off, _ := ctx.Value(ipPrivacyOff{}).(bool); !off {
			input = redactRequest(input)
		}
		return next.Invoke(ctx, input, output)
	}
}

// redactRequest 返回打过码的请求副本；不是发消息类的请求原样返回。
func redactRequest(input bin.Encoder) bin.Encoder {
	switch request := input.(type) {
	case *tg.MessagesSendMessageRequest:
		copied := *request
		message, entities, changed := redactMessage(request.Message, request.Entities)
		copied.Message, copied.Entities = message, entities
		if changed {
			// 远程生成的链接预览可能带出正文里已经遮掉的内容。
			copied.NoWebpage = true
		}
		return &copied
	case *tg.MessagesEditMessageRequest:
		copied := *request
		message, entities, changed := redactMessage(request.Message, request.Entities)
		copied.Message, copied.Entities = message, entities
		if changed {
			copied.NoWebpage = true
		}
		if request.Media != nil {
			copied.Media = redactMedia(request.Media)
		}
		return &copied
	case *tg.MessagesSendMediaRequest:
		copied := *request
		copied.Message, copied.Entities, _ = redactMessage(request.Message, request.Entities)
		copied.Media = redactMedia(request.Media)
		return &copied
	case *tg.MessagesSendMultiMediaRequest:
		copied := *request
		copied.MultiMedia = make([]tg.InputSingleMedia, len(request.MultiMedia))
		for index, item := range request.MultiMedia {
			item.Message, item.Entities, _ = redactMessage(item.Message, item.Entities)
			item.Media = redactMedia(item.Media)
			copied.MultiMedia[index] = item
		}
		return &copied
	}
	return input
}
