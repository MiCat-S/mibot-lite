// Package tts 实现 .t：用 fish.audio 把文字转成语音，或者做成一首带封面的 MP3。
// .ts 管理角色、.tk 设 API Key，和 MiBox 的 t 插件一样。
//
// 数据文件 data/t.json 沿用 MiBox 的 tts_data.json 格式：用 --import-mibox
// 搬过来就能直接用，API Key、当前角色、自己加的角色和封面都在。
package tts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/media"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// endpoint 是 fish.audio 的合成接口。可以用环境变量 MIBOT_FISH_ENDPOINT 换掉，测试用。
const endpoint = "https://api.fish.audio/v1/tts"

// maxText 是一次合成的字数上限。fish.audio 按字数计费，回复一条长消息时
// 一不小心就是一大笔。
const maxText = 1000

// userConfig 是某个账号的设置，字段名照 MiBox。
type userConfig struct {
	APIKey        string `json:"apiKey"`
	DefaultRole   string `json:"defaultRole"`
	DefaultRoleID string `json:"defaultRoleId"`
}

// document 是 data/t.json。Users 按账号 ID 存，这里只用本账号那一项；
// 其余的是从 MiBox 带过来的、别人借用时设的，原样保留。
type document struct {
	Users  map[string]userConfig `json:"users"`
	Roles  map[string]string     `json:"roles"`
	Covers map[string]string     `json:"covers,omitempty"`
	// Model 是 fish.audio 的 model 请求头，空的话由接口决定。MiBox 没有这一项。
	Model string `json:"model,omitempty"`
}

// defaultRole 是还没选过角色时用的，和 MiBox 一样。
const defaultRole = "雷军"

// builtinRoles 是 MiBox 插件里自带的角色：名字 → fish.audio 上的模型 ID。
var builtinRoles = map[string]string{
	"薯薯": "cc1c9874effe4526883662166456513c", "麦当劳": "4066d617322e41abb30ed70eaeaf273f",
	"影视飓风": "91648d8a8d9841c5a1c54fb18e54ab04", "丁真": "54a5170264694bfc8e9ad98df7bd89c3",
	"雷军": "aebaa2305aa2452fbdc8f41eec852a79", "蔡徐坤": "e4642e5edccd4d9ab61a69e82d4f8a14",
	"邓紫棋": "3b55b3d84d2f453a98d8ca9bb24182d6", "周杰伦": "1512d05841734931bf905d0520c272b1",
	"周星驰": "faa3273e5013411199abc13d8f3d6445", "孙笑川": "e80ea225770f42f79d50aa98be3cedfc",
	"央视配音": "59cb5986671546eaa6ca8ae6f29f6d22", "阿诺": "daeda14f742f47b8ac243ccf21c62df8",
	"卢本伟": "24d524b57c5948f598e9b74c4dacc7ab", "电棍": "25d496c425d14109ba4958b6e47ea037",
	"炫狗": "b48533d37bed4ef4b9ad5b11d8b0b694", "阿梓": "c2a6125240f343498e26a9cf38db87b7",
	"七海": "a7725771e0974eb5a9b044ba357f6e13", "嘉然": "1d11381f42b54487b895486f69fb14fb",
	"东雪莲": "7af4d620be1c4c6686132f21940d51c5", "永雏塔菲": "e1cfccf59a1c4492b5f51c7c62a8abd2",
	"可莉": "626bb6d3f3364c9cbc3aa6a67300a664", "刻晴": "5611bf78886a4a9998f56538c4ec7d8c",
	"烧姐姐": "60d377ebaae44829ad4425033b94fdea", "AD学姐": "7f92f8afb8ec43bf81429cc1c9199cb1",
	"御姐": "f44181a3d6d444beae284ad585a1af37", "台湾女": "e855dc04a51f48549b484e41c4d4d4cc",
	"御女茉莉": "6ce7ea8ada884bf3889fa7c7fb206691", "真实女声": "c189c7cff21c400ba67592406202a3a0",
	"女大学生": "5c353fdb312f4888836a9a5680099ef0", "温情女学生": "a1417155aa234890aab4a18686d12849",
	"蒋介石": "918a8277663d476b95e2c4867da0f6a6", "李云龙": "2e576989a8f94e888bf218de90f8c19a",
	"姜文": "ee58439a2e354525bd8fa79380418f4d", "黑手": "f7561ff309bd4040a59f1e600f4f4338",
	"马保国": "794ed17659b243f69cfe6838b03fd31a", "罗永浩": "9cc8e9b9d9ed471a82144300b608bf7f",
	"祁同伟": "4729cb883a58431996b998f2fca7f38b", "郭继承": "ecf03a0cf954498ca0005c472ce7b141",
	"麦克阿瑟": "405736979e244634914add64e37290b0", "营销号": "9d2a825024ce4156a16ba3ff799c4554",
	"蜡笔小新": "60b9a847ba6e485fa8abbde1b9470bc4", "奶龙": "3d1cb00d75184099992ddbaf0fdd7387",
	"懒羊羊": "131c6b3a889543139680d8b3aa26b98d", "剑魔": "ffb55be33cbb4af19b07e9a0ef64dab1",
	"小明剑魔": "a9372068ed0740b48326cf9a74d7496a", "唐僧": "0fb04af381e845e49450762bc941508c",
	"孙悟空": "8d96d5525334476aa67677fb43059dc5", "王琨": "4f201abba2574feeae11e5ebf737859e",
	"麦辣鸡腿堡": "c293697468924f3089cd9b90520dbc16", "猪八戒": "4313e3ec56f14eb3946630dbdad01059",
	"夏(中配) 蔚蓝档案": "c5fca4f670214e3cb7fbb9d595552e6e", "蔚蓝档案阿洛娜": "6ec8168d8392467c82358a780b35c5ca",
	"蔚蓝档案星野": "057265ac020c41a9a91d57c747d3b4c0",
}

// builtinCovers 是自带的封面，和 MiBox 一样只有一个。
var builtinCovers = map[string]string{"薯薯": "https://raw.githubusercontent.com/Yu9191/-/main/image.png"}

// roles 是全部可用角色：自带的，加上文件里的（同名以文件为准）。
func (d document) roles() map[string]string {
	merged := make(map[string]string, len(builtinRoles)+len(d.Roles))
	for name, id := range builtinRoles {
		merged[name] = id
	}
	for name, id := range d.Roles {
		merged[name] = id
	}
	return merged
}

// roleNames 按固定顺序列出角色：自带的在前，自己加的在后，各自按字典序。
func (d document) roleNames() []string {
	var own, builtin []string
	for name := range d.roles() {
		if _, ok := builtinRoles[name]; ok {
			builtin = append(builtin, name)
		} else {
			own = append(own, name)
		}
	}
	sort.Strings(builtin)
	sort.Strings(own)
	return append(builtin, own...)
}

func (d document) cover(role string) string {
	if link, ok := d.Covers[role]; ok {
		return link
	}
	return builtinCovers[role]
}

// voice 是本账号当前用的角色名和模型 ID。
func (d document) voice(self string) (string, string) {
	user := d.Users[self]
	if user.DefaultRole != "" && user.DefaultRoleID != "" {
		return user.DefaultRole, user.DefaultRoleID
	}
	return defaultRole, d.roles()[defaultRole]
}

var (
	// emojiRanges 是要去掉的表情符号，范围照 MiBox。
	emojiRanges = regexp.MustCompile(`[\x{1F600}-\x{1F64F}\x{1F300}-\x{1F5FF}\x{1F680}-\x{1F6FF}\x{2600}-\x{26FF}\x{2700}-\x{27BF}\x{FE0F}\x{200D}]`)
	// notSpoken 是白名单以外的字符：只留汉字、字母、数字、空白和常用标点。
	notSpoken = regexp.MustCompile(`[^\x{4e00}-\x{9fa5}a-zA-Z0-9\s，。？！、,?!.]`)
	// voiceIDPattern 是 fish.audio 模型 ID 的样子：32 位十六进制。
	voiceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	modelPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)
)

// cleanText 照 MiBox 清理要念的文字：去掉表情和白名单以外的符号，连续的同一个标点只留一个。
func cleanText(text string) string {
	text = emojiRanges.ReplaceAllString(text, "")
	text = notSpoken.ReplaceAllString(text, "")
	var out strings.Builder
	var previous rune
	for _, r := range text {
		if r == previous && strings.ContainsRune("，。？！、,?!.", r) {
			continue
		}
		out.WriteRune(r)
		previous = r
	}
	return strings.TrimSpace(out.String())
}

// synthesize 调 fish.audio 合成，返回 MP3。
func synthesize(ctx context.Context, config document, key, voiceID, text string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"text": text, "reference_id": voiceID, "format": "mp3"})
	headers := map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}
	if config.Model != "" {
		headers["model"] = config.Model
	}
	target := endpoint
	if override := os.Getenv("MIBOT_FISH_ENDPOINT"); override != "" {
		target = override
	}
	response, err := httpx.Do(ctx, httpx.Request{Method: "POST", URL: target, Headers: headers, Body: body,
		Timeout: 2 * time.Minute, MaxBytes: 20 << 20})
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, apiError(response)
	}
	if len(response.Body) == 0 {
		return nil, kit.Fail("fish.audio 返回了空的音频")
	}
	return response.Body, nil
}

// apiError 把 fish.audio 的错误应答翻成一句话。它的错误体通常是 {"message": "..."} 或 {"detail": ...}。
func apiError(response httpx.Response) error {
	var detail struct {
		Message string `json:"message"`
		Detail  any    `json:"detail"`
	}
	_ = json.Unmarshal(response.Body, &detail)
	reason := detail.Message
	if reason == "" && detail.Detail != nil {
		reason = fmt.Sprint(detail.Detail)
	}
	if reason == "" {
		reason = strings.TrimSpace(string(response.Body))
	}
	if utf8.RuneCountInString(reason) > 200 {
		reason = string([]rune(reason)[:200]) + "…"
	}
	switch response.Status {
	case 401, 403:
		return kit.Fail("API Key 无效（" + strconv.Itoa(response.Status) + "）")
	case 402:
		return kit.Fail("fish.audio 账户余额不足")
	}
	if reason == "" {
		return kit.Failf("fish.audio 返回 HTTP %d", response.Status)
	}
	return kit.Failf("fish.audio 返回 HTTP %d：%s", response.Status, reason)
}

// downloadCover 下载封面，只接受 https 链接，最大 5 MB；失败就不加封面。
func downloadCover(ctx context.Context, link string) []byte {
	if parsed, err := url.Parse(link); err != nil || parsed.Scheme != "https" {
		return nil
	}
	response, err := httpx.Do(ctx, httpx.Request{URL: link, Timeout: 30 * time.Second, MaxBytes: 5 << 20})
	if err != nil || !response.OK() {
		return nil
	}
	return response.Body
}

func help(prefix string) string {
	p := command.Escape(prefix)
	return "🔊 <b>文字转语音</b>\n\n用 fish.audio 的角色声音念一段话，发成语音消息。\n\n" +
		"• <code>" + p + "t 文本</code> 发语音；回复一条消息只发 <code>" + p + "t</code> 念那条消息\n" +
		"• <code>" + p + "t song 歌名 歌手 文本</code> 做成带封面的 MP3，专辑名是当前角色\n" +
		"• <code>" + p + "t fm 封面链接</code> 给当前角色设封面\n" +
		"• <code>" + p + "ts [页码]</code> 看角色列表，<code>" + p + "ts 角色名</code> 切换，" +
		"<code>" + p + "ts 角色名 模型ID</code> 新增并切换\n" +
		"• <code>" + p + "tk API密钥</code> 设 API Key（发出后命令消息会被改掉，密钥不留在聊天里）\n" +
		"• <code>" + p + "t model [名称]</code> 看或设 fish.audio 的模型，如 s1、s2-pro、s2.1-pro；" +
		"<code>" + p + "t model default</code> 交给接口决定\n\n" +
		"API Key 在 https://fish.audio/ 申请，更多角色见 https://fish.audio/zh-CN/app/discovery/ 。" +
		"一次最多 " + strconv.Itoa(maxText) + " 字，表情和生僻符号会被去掉。只有你本人能用：按字数计费，不借给别人。"
}

type service struct {
	a     *app.App
	store *store.Store[document]
}

func (s *service) self(inv *command.Invocation) string {
	return strconv.FormatInt(inv.Client.SelfID(), 10)
}

// Register 注册 .t、.ts 和 .tk。
func Register(a *app.App) {
	s := &service{a: a, store: kit.NewStore(a, "t.json", func() document {
		return document{Users: map[string]userConfig{}, Roles: map[string]string{}}
	})}
	a.Registry.Register(
		&command.Command{Name: "t", Description: "文字转语音", Usage: "文本 | song 歌名 歌手 文本 | fm 链接 | model", Help: help,
			Timeout: 3 * time.Minute, Handle: s.speak},
		&command.Command{Name: "ts", Description: "t 的角色管理", Usage: "[页码|角色名 [模型ID]]", Help: help, Handle: s.roles},
		&command.Command{Name: "tk", Description: "设置 t 的 API Key", Usage: "API密钥", Help: help, Handle: s.key},
	)
}

func (s *service) speak(ctx context.Context, inv *command.Invocation) error {
	switch strings.ToLower(inv.Arg(0)) {
	case "help", "h":
		return inv.Edit(ctx, help(inv.Prefix))
	case "fm":
		return s.setCover(ctx, inv, inv.Arg(1))
	case "model":
		return s.model(ctx, inv, inv.Arg(1))
	}
	config, err := s.store.Read()
	if err != nil {
		return err
	}
	user := config.Users[s.self(inv)]
	if user.APIKey == "" {
		return inv.Edit(ctx, "❌ 还没设 API Key，先发 "+command.Code(inv.Prefix+"tk 你的密钥")+"。\n\n"+help(inv.Prefix))
	}
	song := strings.EqualFold(inv.Arg(0), "song")
	var title, artist, text string
	if song {
		title, artist, text = inv.Arg(1), inv.Arg(2), strings.TrimSpace(inv.Rest(3))
		if title == "" || artist == "" {
			return inv.EditText(ctx, "用法："+inv.Prefix+"t song 歌名 歌手 文本")
		}
	} else {
		text = strings.TrimSpace(inv.Rest(0))
	}
	if text == "" && inv.Message.ReplyToID != 0 {
		if reply, err := inv.Client.GetReply(ctx, inv.Message); err == nil && reply != nil {
			text = reply.Text
		}
	}
	if text == "" {
		return inv.Edit(ctx, help(inv.Prefix))
	}
	text = cleanText(text)
	if text == "" {
		return inv.EditText(ctx, "❌ 去掉表情和符号之后没有能念的字了")
	}
	if count := utf8.RuneCountInString(text); count > maxText {
		return inv.EditText(ctx, fmt.Sprintf("❌ 太长了：%d 字，一次最多 %d 字", count, maxText))
	}
	role, voiceID := config.voice(s.self(inv))
	if err := inv.EditText(ctx, "🔊 "+role+" 正在念…"); err != nil {
		return err
	}
	audio, err := synthesize(ctx, config, user.APIKey, voiceID, text)
	if err != nil {
		if reason, ok := kit.IsUserError(err); ok {
			return inv.EditText(ctx, "❌ "+reason)
		}
		return inv.EditText(ctx, "❌ 合成失败："+httpx.Reason(err))
	}
	directory, err := os.MkdirTemp("", "mibot-tts-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	peer, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	var options bot.DocumentOptions
	var data []byte
	if song {
		data, err = media.TaggedMP3(ctx, directory, audio, media.Tags{Title: title, Artist: artist, Album: role,
			Cover: downloadCover(ctx, config.cover(role))})
		if err != nil {
			return inv.EditText(ctx, "❌ 音频处理失败："+command.Brief(err))
		}
		options = bot.DocumentOptions{Name: title + ".mp3", MimeType: "audio/mpeg", Caption: command.Escape(title + " - " + artist),
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Title: title, Performer: artist,
				Duration: media.Duration(ctx, directory, "song.mp3")}}}
	} else {
		data, err = media.VoiceOgg(ctx, directory, audio)
		if err != nil {
			return inv.EditText(ctx, "❌ 音频处理失败："+command.Brief(err))
		}
		options = bot.DocumentOptions{Name: "voice.ogg", MimeType: "audio/ogg",
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Voice: true,
				Duration: media.Duration(ctx, directory, "voice.ogg")}}}
	}
	options.ReplyTo = inv.Message.ReplyToID
	if err := inv.Client.SendDocumentWith(ctx, peer, data, options); err != nil {
		return err
	}
	return inv.Client.DeleteMessage(ctx, inv.Message)
}

// roles 是 .ts：看角色列表（分页）、切换角色、新增角色。
func (s *service) roles(ctx context.Context, inv *command.Invocation) error {
	config, err := s.store.Read()
	if err != nil {
		return err
	}
	self := s.self(inv)
	current, _ := config.voice(self)
	page, pageErr := strconv.Atoi(inv.Arg(0))
	if inv.Arg(0) == "" || (pageErr == nil && inv.Arg(1) == "") {
		const size = 20
		names := config.roleNames()
		pages := max(1, (len(names)+size-1)/size)
		page = kit.Clamp(max(page, 1), 1, pages)
		lines := []string{fmt.Sprintf("🎭 <b>角色</b>（%d 个）· 第 %d/%d 页", len(names), page, pages),
			"当前：" + command.Escape(current), ""}
		for index := (page - 1) * size; index < min(page*size, len(names)); index++ {
			lines = append(lines, strconv.Itoa(index+1)+". "+command.Escape(names[index]))
		}
		lines = append(lines, "", command.Code(inv.Prefix+"ts 角色名")+" 切换 · "+
			command.Code(inv.Prefix+"ts 角色名 模型ID")+" 新增 · "+command.Code(inv.Prefix+"ts 2")+" 下一页")
		return inv.Edit(ctx, strings.Join(lines, "\n"))
	}
	name, id := inv.Arg(0), inv.Arg(1)
	if id == "" {
		known, ok := config.roles()[name]
		if !ok {
			return inv.Edit(ctx, "❌ 没有这个角色："+command.Escape(name)+"\n用 "+command.Code(inv.Prefix+"ts 角色名 模型ID")+" 新增")
		}
		id = known
	} else if !voiceIDPattern.MatchString(id) {
		return inv.EditText(ctx, "❌ 模型 ID 是 fish.audio 上 32 位的十六进制串")
	}
	added := inv.Arg(1) != ""
	if err := s.store.Update(func(value *document) error {
		if value.Users == nil {
			value.Users = map[string]userConfig{}
		}
		if added {
			if value.Roles == nil {
				value.Roles = map[string]string{}
			}
			value.Roles[name] = id
		}
		user := value.Users[self]
		user.DefaultRole, user.DefaultRoleID = name, id
		value.Users[self] = user
		return nil
	}); err != nil {
		return err
	}
	if added {
		return inv.Edit(ctx, "✅ 已新增角色 "+command.Escape(name)+" 并切换过去（"+command.Code(id)+"）")
	}
	return inv.Edit(ctx, "✅ 当前角色："+command.Escape(name))
}

// key 是 .tk：设 API Key。命令消息立刻改掉，密钥不留在聊天记录里。
func (s *service) key(ctx context.Context, inv *command.Invocation) error {
	value := strings.TrimSpace(inv.Arg(0))
	if value == "" {
		return inv.EditText(ctx, "用法："+inv.Prefix+"tk API密钥（在 https://fish.audio/ 申请）")
	}
	if err := inv.EditText(ctx, "⏳ 正在保存…"); err != nil {
		return err
	}
	self := s.self(inv)
	if err := s.store.Update(func(config *document) error {
		if config.Users == nil {
			config.Users = map[string]userConfig{}
		}
		user := config.Users[self]
		user.APIKey = value
		if user.DefaultRole == "" {
			user.DefaultRole, user.DefaultRoleID = defaultRole, builtinRoles[defaultRole]
		}
		config.Users[self] = user
		return nil
	}); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ API Key 已保存")
}

func (s *service) setCover(ctx context.Context, inv *command.Invocation, link string) error {
	if parsed, err := url.Parse(link); err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return inv.EditText(ctx, "用法："+inv.Prefix+"t fm https://图片链接")
	}
	config, err := s.store.Read()
	if err != nil {
		return err
	}
	role, _ := config.voice(s.self(inv))
	if err := s.store.Update(func(value *document) error {
		if value.Covers == nil {
			value.Covers = map[string]string{}
		}
		value.Covers[role] = link
		return nil
	}); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ 已给角色 "+role+" 设置封面")
}

func (s *service) model(ctx context.Context, inv *command.Invocation, name string) error {
	if name == "" {
		config, err := s.store.Read()
		if err != nil {
			return err
		}
		current := config.Model
		if current == "" {
			current = "未指定（接口默认）"
		}
		return inv.Edit(ctx, "当前模型："+command.Code(current)+"\n可选如 s1、s2-pro、s2.1-pro、s2.1-pro-free，以 fish.audio 文档为准。")
	}
	if strings.EqualFold(name, "default") {
		name = ""
	} else if !modelPattern.MatchString(name) {
		return inv.EditText(ctx, "❌ 模型名只能有字母、数字和 . _ -")
	}
	if err := s.store.Update(func(value *document) error { value.Model = name; return nil }); err != nil {
		return err
	}
	if name == "" {
		return inv.EditText(ctx, "✅ 模型交给接口决定")
	}
	return inv.Edit(ctx, "✅ 模型设为 "+command.Code(name))
}
