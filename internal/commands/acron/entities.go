package acron

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// storedEntity 是一个格式实体按 teleproto 的 JSON.stringify 写出来的样子：offset、length、
// 各类型自己的字段，最后是类名 className。64 位整数（userId、documentId）是十进制字符串，
// 和 V2 的 serializeMessageEntities 写的一样，两边的数据文件可以互相读。
type storedEntity struct {
	Offset     int    `json:"offset"`
	Length     int    `json:"length"`
	URL        string `json:"url,omitempty"`
	Language   string `json:"language,omitempty"`
	UserID     string `json:"userId,omitempty"`
	DocumentID string `json:"documentId,omitempty"`
	Collapsed  bool   `json:"collapsed,omitempty"`
	ClassName  string `json:"className"`
}

// encodeEntities 把消息的格式写成 V2 的 TL JSON。V2 只存 MessageEntity 开头的类，
// 这里也只写它们对应的那些；gotd 比 teleproto 新出来的几种不写，一个都没有时返回 nil。
func encodeEntities(entities []tg.MessageEntityClass) json.RawMessage {
	var list []storedEntity
	for _, entity := range entities {
		stored := storedEntity{Offset: entity.GetOffset(), Length: entity.GetLength()}
		switch value := entity.(type) {
		case *tg.MessageEntityUnknown:
			stored.ClassName = "MessageEntityUnknown"
		case *tg.MessageEntityMention:
			stored.ClassName = "MessageEntityMention"
		case *tg.MessageEntityHashtag:
			stored.ClassName = "MessageEntityHashtag"
		case *tg.MessageEntityBotCommand:
			stored.ClassName = "MessageEntityBotCommand"
		case *tg.MessageEntityURL:
			stored.ClassName = "MessageEntityUrl"
		case *tg.MessageEntityEmail:
			stored.ClassName = "MessageEntityEmail"
		case *tg.MessageEntityBold:
			stored.ClassName = "MessageEntityBold"
		case *tg.MessageEntityItalic:
			stored.ClassName = "MessageEntityItalic"
		case *tg.MessageEntityCode:
			stored.ClassName = "MessageEntityCode"
		case *tg.MessageEntityPre:
			stored.ClassName, stored.Language = "MessageEntityPre", value.Language
		case *tg.MessageEntityTextURL:
			stored.ClassName, stored.URL = "MessageEntityTextUrl", value.URL
		case *tg.MessageEntityMentionName:
			stored.ClassName, stored.UserID = "MessageEntityMentionName", strconv.FormatInt(value.UserID, 10)
		case *tg.MessageEntityPhone:
			stored.ClassName = "MessageEntityPhone"
		case *tg.MessageEntityCashtag:
			stored.ClassName = "MessageEntityCashtag"
		case *tg.MessageEntityUnderline:
			stored.ClassName = "MessageEntityUnderline"
		case *tg.MessageEntityStrike:
			stored.ClassName = "MessageEntityStrike"
		case *tg.MessageEntityBankCard:
			stored.ClassName = "MessageEntityBankCard"
		case *tg.MessageEntitySpoiler:
			stored.ClassName = "MessageEntitySpoiler"
		case *tg.MessageEntityCustomEmoji:
			stored.ClassName, stored.DocumentID = "MessageEntityCustomEmoji", strconv.FormatInt(value.DocumentID, 10)
		case *tg.MessageEntityBlockquote:
			stored.ClassName, stored.Collapsed = "MessageEntityBlockquote", value.Collapsed
		default:
			continue
		}
		list = append(list, stored)
	}
	if len(list) == 0 {
		return nil
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return raw
}

// decodeEntities 把存下的 TL JSON 读回格式实体。认不得的类名、缺字段的条目跳过，
// 不让一条坏数据拦住整条消息，同 V2 的 reviveMessageEntities。数字字段写成数字或字符串都认。
func decodeEntities(raw json.RawMessage) []tg.MessageEntityClass {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var items []map[string]any
	if decoder.Decode(&items) != nil {
		return nil
	}
	var entities []tg.MessageEntityClass
	for _, item := range items {
		offset, okOffset := intField(item["offset"])
		length, okLength := intField(item["length"])
		if !okOffset || !okLength || offset < 0 || length <= 0 {
			continue
		}
		o, l := int(offset), int(length)
		var entity tg.MessageEntityClass
		switch name, _ := item["className"].(string); name {
		case "MessageEntityUnknown":
			entity = &tg.MessageEntityUnknown{Offset: o, Length: l}
		case "MessageEntityMention":
			entity = &tg.MessageEntityMention{Offset: o, Length: l}
		case "MessageEntityHashtag":
			entity = &tg.MessageEntityHashtag{Offset: o, Length: l}
		case "MessageEntityBotCommand":
			entity = &tg.MessageEntityBotCommand{Offset: o, Length: l}
		case "MessageEntityUrl":
			entity = &tg.MessageEntityURL{Offset: o, Length: l}
		case "MessageEntityEmail":
			entity = &tg.MessageEntityEmail{Offset: o, Length: l}
		case "MessageEntityBold":
			entity = &tg.MessageEntityBold{Offset: o, Length: l}
		case "MessageEntityItalic":
			entity = &tg.MessageEntityItalic{Offset: o, Length: l}
		case "MessageEntityCode":
			entity = &tg.MessageEntityCode{Offset: o, Length: l}
		case "MessageEntityPre":
			language, _ := item["language"].(string)
			entity = &tg.MessageEntityPre{Offset: o, Length: l, Language: language}
		case "MessageEntityTextUrl":
			url, _ := item["url"].(string)
			if url == "" {
				continue
			}
			entity = &tg.MessageEntityTextURL{Offset: o, Length: l, URL: url}
		case "MessageEntityMentionName":
			id, ok := intField(item["userId"])
			if !ok {
				continue
			}
			entity = &tg.MessageEntityMentionName{Offset: o, Length: l, UserID: id}
		case "MessageEntityPhone":
			entity = &tg.MessageEntityPhone{Offset: o, Length: l}
		case "MessageEntityCashtag":
			entity = &tg.MessageEntityCashtag{Offset: o, Length: l}
		case "MessageEntityUnderline":
			entity = &tg.MessageEntityUnderline{Offset: o, Length: l}
		case "MessageEntityStrike":
			entity = &tg.MessageEntityStrike{Offset: o, Length: l}
		case "MessageEntityBankCard":
			entity = &tg.MessageEntityBankCard{Offset: o, Length: l}
		case "MessageEntitySpoiler":
			entity = &tg.MessageEntitySpoiler{Offset: o, Length: l}
		case "MessageEntityCustomEmoji":
			id, ok := intField(item["documentId"])
			if !ok {
				continue
			}
			entity = &tg.MessageEntityCustomEmoji{Offset: o, Length: l, DocumentID: id}
		case "MessageEntityBlockquote":
			collapsed, _ := item["collapsed"].(bool)
			entity = &tg.MessageEntityBlockquote{Offset: o, Length: l, Collapsed: collapsed}
		default:
			continue
		}
		entities = append(entities, entity)
	}
	return entities
}

// intField 读 JSON 里的整数：数字或十进制字符串。
func intField(value any) (int64, bool) {
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case string:
		text = typed
	default:
		return 0, false
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	return parsed, err == nil
}

// inputEntities 把格式实体换成发送时用的样子。提及某人（MentionName）发送时要带那个人的
// access hash，缓存里查得到才换成 InputMessageEntityMentionName，查不到就只留文字，
// 不造一个会被拒的提及，和 bot 包解析 HTML 时的做法一样。
func inputEntities(peers *bot.PeerCache, entities []tg.MessageEntityClass) []tg.MessageEntityClass {
	result := make([]tg.MessageEntityClass, 0, len(entities))
	for _, entity := range entities {
		mention, ok := entity.(*tg.MessageEntityMentionName)
		if !ok {
			result = append(result, entity)
			continue
		}
		if peers == nil {
			continue
		}
		input, known := peers.InputPeer(&tg.PeerUser{UserID: mention.UserID})
		if !known {
			continue
		}
		user, ok := bot.InputUser(input)
		if !ok {
			continue
		}
		result = append(result, &tg.InputMessageEntityMentionName{Offset: mention.Offset, Length: mention.Length, UserID: user})
	}
	return result
}
