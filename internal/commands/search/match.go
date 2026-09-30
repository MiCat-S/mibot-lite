package search

import (
	"regexp"
	"strings"

	"github.com/gotd/td/tg"
)

// 这里是和 Telegram 无关的判断：哪些消息算视频、叫什么、多长、是不是广告、和关键词
// 匹配得怎样。规则与 MiBox 的 search/v2/videos.ts 一致。

// Go 的 \s 只认 ASCII 空白，JavaScript 的 \s 还包括全角空格（U+3000）、不换行空格这些 Unicode
// 空白和 U+FEFF。频道文字里常有全角空格，所以这里的空白都写成 [\s\p{Z}\x{FEFF}]，与 MiBox 一致。
var (
	separators = regexp.MustCompile(`[-_\s\p{Z}\x{FEFF}.|\\/#]+`)
	spaces     = regexp.MustCompile(`[\s\p{Z}\x{FEFF}]+`)
	// letterDigits 认「字母后面跟数字」的番号式关键词（abc123、abc 123），这种关键词去掉空格再比。
	letterDigits = regexp.MustCompile(`(?i)[a-z]+[\s\p{Z}\x{FEFF}]*\d+`)
)

// normalize 转小写，把 - _ . | \ / # 和空白都当成一个空格。
func normalize(text string) string {
	text = separators.ReplaceAllString(strings.ToLower(text), " ")
	return strings.TrimSpace(spaces.ReplaceAllString(text, " "))
}

// fuzzyMatch 判断规整过的 text 是否匹配规整过的 query：整体包含；番号式的单词去掉空格后包含；
// 或者关键词的每个词都出现在某个词里。
func fuzzyMatch(text, query string) bool {
	if strings.Contains(text, query) {
		return true
	}
	parts := strings.Fields(query)
	words := strings.Split(text, " ")
	if len(parts) == 1 && letterDigits.MatchString(query) {
		if strings.Contains(strings.ReplaceAll(text, " ", ""), strings.ReplaceAll(query, " ", "")) {
			return true
		}
	}
	for _, part := range parts {
		found := false
		for _, word := range words {
			if strings.Contains(word, part) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// videoDocument 返回消息里的视频文档和它的视频属性。和 teleproto 的 message.video 一样，
// 带 DocumentAttributeVideo 的文档都算（包括圆形视频和动图）；链接预览里的视频不算。
func videoDocument(message *tg.Message) (*tg.Document, *tg.DocumentAttributeVideo, bool) {
	media, ok := message.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, nil, false
	}
	document, ok := media.Document.(*tg.Document)
	if !ok {
		return nil, nil, false
	}
	for _, attribute := range document.Attributes {
		if video, ok := attribute.(*tg.DocumentAttributeVideo); ok {
			return document, video, true
		}
	}
	return nil, nil, false
}

// fileName 是视频文档声明的文件名，没有视频或没有文件名时为 ""。
func fileName(message *tg.Message) string {
	document, _, ok := videoDocument(message)
	if !ok {
		return ""
	}
	for _, attribute := range document.Attributes {
		if name, ok := attribute.(*tg.DocumentAttributeFilename); ok {
			return name.FileName
		}
	}
	return ""
}

// duration 是视频时长（秒），不是视频时为 0。
func duration(message *tg.Message) float64 {
	if _, video, ok := videoDocument(message); ok {
		return video.Duration
	}
	return 0
}

// matches 判断消息的文字或视频文件名是否匹配关键词。
func matches(message *tg.Message, query string) bool {
	normalized := normalize(query)
	for _, source := range []string{message.Message, fileName(message)} {
		if source != "" && fuzzyMatch(normalize(source), normalized) {
			return true
		}
	}
	return false
}

// score 给匹配的结果排序：文件名包含整个关键词加 100，文字包含加 50。
func score(message *tg.Message, query string) int {
	normalized := normalize(query)
	total := 0
	if name := fileName(message); name != "" && strings.Contains(normalize(name), normalized) {
		total += 100
	}
	if message.Message != "" && strings.Contains(normalize(message.Message), normalized) {
		total += 50
	}
	return total
}

// isAd 判断消息的文字或文件名里有没有广告过滤词（不分大小写）。空的过滤词不算，
// 否则它会匹配所有消息。
func isAd(message *tg.Message, filters []string) bool {
	text := strings.ToLower(message.Message + "\n" + fileName(message))
	for _, word := range filters {
		if word != "" && strings.Contains(text, strings.ToLower(word)) {
			return true
		}
	}
	return false
}

// options 是搜索参数：-s 防剧透、-r 随机（不分大小写，出现在哪里都行），剩下的是关键词；
// 剩下的第一个词是 kkp 时是随机速览。
type options struct {
	spoiler, random, kkp bool
	query                string
}

func parseOptions(args []string, forcedKKP bool) options {
	var result options
	var rest []string
	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "-s":
			result.spoiler = true
		case "-r":
			result.random = true
		default:
			rest = append(rest, arg)
		}
	}
	result.kkp = forcedKKP || (len(rest) > 0 && strings.ToLower(rest[0]) == "kkp")
	if !result.kkp {
		result.query = strings.Join(rest, " ")
	}
	return result
}
