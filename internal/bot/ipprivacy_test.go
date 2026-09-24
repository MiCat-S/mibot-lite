package bot

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestMaskIPs(t *testing.T) {
	t.Cleanup(func() { _ = SetIPPolicy(DefaultIPPolicy) })
	for input, want := range map[string]string{
		"出口 1.2.3.4 端口":          "出口 1.2.*.* 端口",
		"http://10.0.0.8:8080/x": "http://10.0.*.*:8080/x",
		"v6 2001:db8::1 结束":      "v6 2001:db8:0:0:*:*:*:* 结束",
		"映射 ::ffff:1.2.3.4":      "映射 0:0:0:0:*:*:*:*",
		"两个 8.8.8.8、1.1.1.1。":    "两个 8.8.*.*、1.1.*.*。",
		"版本 v1.2.3.4":            "版本 v1.2.3.4",
		"时间 12:30:45":            "时间 12:30:45",
		"MAC aa:bb:cc:dd:ee:ff":  "MAC aa:bb:cc:dd:ee:ff",
		"超范围 256.1.1.1":          "超范围 256.1.1.1",
		"五段 1.2.3.4.5":           "五段 1.2.3.4.5",
		"前导零 01.2.3.4":           "前导零 01.2.3.4",
		"句末 1.2.3.4.":            "句末 1.2.*.*.",
		"区域 fe80::1%eth0 后":      "区域 fe80:0:0:0:*:*:*:* 后",
	} {
		if got := MaskIPs(input); got != want {
			t.Errorf("MaskIPs(%q) = %q，应为 %q", input, got, want)
		}
	}
	if err := SetIPPolicy(IPPolicy{Mode: "hide", IPv4: 2, IPv6: 4}); err != nil {
		t.Fatal(err)
	}
	if got := MaskIPs("a 1.2.3.4 b"); got != "a [IP已隐藏] b" {
		t.Errorf("隐藏模式：%q", got)
	}
	if err := SetIPPolicy(IPPolicy{Mode: "mask", IPv4: 4, IPv6: 8}); err != nil {
		t.Fatal(err)
	}
	if got := MaskIPs("1.2.3.4"); got != "*.*.*.*" {
		t.Errorf("遮 4 段：%q", got)
	}
	if SetIPPolicy(IPPolicy{Mode: "mask", IPv4: 5, IPv6: 4}) == nil || SetIPPolicy(IPPolicy{Mode: "off", IPv4: 2, IPv6: 4}) == nil {
		t.Error("超范围的设置应该被拒")
	}
}

// 打码后长度变了，后面的格式实体要跟着挪；emoji 在 UTF-16 里占两个单位。
func TestRedactMessageEntities(t *testing.T) {
	text := "😀 10.20.30.40 看 链接"
	// 😀=2，空格=1，所以 IP 从 3 开始，长 11；「看」在 15；「链接」在 17。
	entities := []tg.MessageEntityClass{
		&tg.MessageEntityCode{Offset: 3, Length: 11},
		&tg.MessageEntityBold{Offset: 15, Length: 1},
		&tg.MessageEntityTextURL{Offset: 17, Length: 2, URL: "http://10.20.30.40/admin"},
		&tg.MessageEntityItalic{Offset: 17, Length: 2},
	}
	masked, kept, changed := redactMessage(text, entities)
	if masked != "😀 10.20.*.* 看 链接" || !changed {
		t.Fatalf("正文 %q changed=%v", masked, changed)
	}
	if len(kept) != 3 {
		t.Fatalf("应去掉指向 IP 的链接，剩 3 个实体，实际 %d：%+v", len(kept), kept)
	}
	code, bold, italic := kept[0].(*tg.MessageEntityCode), kept[1].(*tg.MessageEntityBold), kept[2].(*tg.MessageEntityItalic)
	if code.Offset != 3 || code.Length != 9 || bold.Offset != 13 || italic.Offset != 15 {
		t.Errorf("实体位置不对：code %d+%d bold %d italic %d", code.Offset, code.Length, bold.Offset, italic.Offset)
	}
	if entities[1].(*tg.MessageEntityBold).Offset != 15 {
		t.Error("不该改动调用方手里的实体")
	}
	plain, same, changed := redactMessage("没有地址 1.2.3", entities[:2])
	if plain != "没有地址 1.2.3" || changed || len(same) != 2 {
		t.Errorf("没有 IP 时应原样返回：%q %v %d", plain, changed, len(same))
	}
}

type capture struct{ input bin.Encoder }

func (c *capture) Invoke(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
	c.input = input
	return nil
}

func TestIPRedactor(t *testing.T) {
	request := &tg.MessagesSendMessageRequest{Message: "出口 1.2.3.4"}
	sink := &capture{}
	handler := IPRedactor{}.Handle(sink)
	if err := handler(context.Background(), request, nil); err != nil {
		t.Fatal(err)
	}
	sent := sink.input.(*tg.MessagesSendMessageRequest)
	if sent.Message != "出口 1.2.*.*" || !sent.NoWebpage {
		t.Errorf("发出去的：%q noWebpage=%v", sent.Message, sent.NoWebpage)
	}
	if request.Message != "出口 1.2.3.4" {
		t.Error("不该改动调用方的请求")
	}
	if err := handler(WithoutIPPrivacy(context.Background()), request, nil); err != nil {
		t.Fatal(err)
	}
	if sink.input.(*tg.MessagesSendMessageRequest).Message != "出口 1.2.3.4" {
		t.Error("标记为不打码的请求应原样发出")
	}
	media := &tg.MessagesSendMediaRequest{Message: "说明 8.8.8.8", Media: &tg.InputMediaUploadedDocument{
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "log-8.8.8.8.txt"}}}}
	_ = handler(context.Background(), media, nil)
	sentMedia := sink.input.(*tg.MessagesSendMediaRequest)
	name := sentMedia.Media.(*tg.InputMediaUploadedDocument).Attributes[0].(*tg.DocumentAttributeFilename).FileName
	if sentMedia.Message != "说明 8.8.*.*" || name != "log-8.8.*.*.txt" {
		t.Errorf("媒体：%q 文件名 %q", sentMedia.Message, name)
	}
	if other := (&tg.MessagesGetHistoryRequest{}); redactRequest(other) != other {
		t.Error("不是发消息的请求应原样放过")
	}
}
