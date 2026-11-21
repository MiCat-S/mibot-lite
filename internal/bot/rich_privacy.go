package bot

import (
	"reflect"

	"github.com/gotd/td/tg"
)

// 富文本的正文、表格和链接都在 Blocks 内。发送和编辑共用这条路径，
// 只复制发生修改的分支，以免改掉复制消息或重试请求使用的原始块。
func redactRichMessage(input tg.InputRichMessageClass) tg.InputRichMessageClass {
	rich, ok := input.(*tg.InputRichMessage)
	if !ok {
		return input
	}
	blocks := redactRichField(reflect.ValueOf(rich.Blocks), "")
	if !blocks.IsValid() {
		return input
	}
	copied := *rich
	copied.Blocks = blocks.Interface().([]tg.PageBlockClass)
	return &copied
}

// gotd 的块、列表项、单元格、行内格式都由 TL 结构体和切片构成。
// 空 Value 表示该字段沿用原值。只遍历富文本块，不遍历照片或文档的文件引用。
func redactRichField(v reflect.Value, name string) reflect.Value {
	var out reflect.Value
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			break
		}
		// 指向 IP 的链接保留标签，去掉跳转动作；显示成多段的 IP 必须作为完整地址判断。
		switch node := v.Interface().(type) {
		case *tg.TextURL:
			if ipLink(node.URL) {
				return reflect.ValueOf(RichPlain(MaskIPs(richPlain(node.Text))))
			}
		case *tg.TextAutoURL:
			if label := richPlain(node.Text); ipLink(label) {
				return reflect.ValueOf(RichPlain(MaskIPs(label)))
			}
		case *tg.TextButton:
			if link, ok := node.Type.(interface{ GetURL() string }); ok && ipLink(link.GetURL()) {
				return reflect.ValueOf(RichPlain(MaskIPs(richPlain(node.Text))))
			}
		case *tg.TextConcat:
			out = redactRichField(v.Elem(), name)
			plain := richPlain(node)
			if masked := MaskIPs(plain); masked != plain {
				if !out.IsValid() || richPlain(out.Interface().(tg.RichTextClass)) != masked {
					return reflect.ValueOf(RichPlain(masked))
				}
			}
			return out
		}
		return redactRichField(v.Elem(), name)
	case reflect.Pointer:
		if !v.IsNil() {
			if child := redactRichField(v.Elem(), name); child.IsValid() {
				out = reflect.New(v.Type().Elem())
				out.Elem().Set(child)
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			child := redactRichField(v.Field(i), v.Type().Field(i).Name)
			if child.IsValid() {
				if !out.IsValid() {
					out = reflect.New(v.Type()).Elem()
					out.Set(v)
				}
				out.Field(i).Set(child)
			}
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			break
		}
		for i := range v.Len() {
			if child := redactRichField(v.Index(i), name); child.IsValid() {
				if !out.IsValid() {
					out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
					reflect.Copy(out, v)
				}
				out.Index(i).Set(child)
			}
		}
	case reflect.String:
		text := v.String()
		if name == "URL" && ipLink(text) {
			return reflect.ValueOf("")
		}
		if masked := MaskIPs(text); masked != text {
			return reflect.ValueOf(masked)
		}
	}
	return out
}
