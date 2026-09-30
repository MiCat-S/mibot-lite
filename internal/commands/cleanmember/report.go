package cleanmember

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/fsutil"
)

// userInfo 是报告里的一个成员，字段名照 MiBox v2 的缓存。
type userInfo struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	IsDeleted bool   `json:"is_deleted"`
	// LastOnline 是离线时刻（ISO 8601），或者 online、recently、lastweek 这样的状态；看不到时为 null。
	LastOnline   *string `json:"last_online"`
	ErrorMessage string  `json:"error_message,omitempty"`
}

// cacheData 是一次搜索的结果，形状同 v2 缓存里的一项。ChatID 是不带 -100 的纯数字
// （v2 的 String(chat.id)），缓存的键也用它。
type cacheData struct {
	ChatID     string     `json:"chat_id"`
	ChatTitle  string     `json:"chat_title"`
	Mode       string     `json:"mode"`
	Day        int        `json:"day"`
	SearchTime string     `json:"search_time"`
	TotalFound int        `json:"total_found"`
	Users      []userInfo `json:"users"`
	// ExpiresAt 是毫秒时间戳。
	ExpiresAt int64 `json:"expiresAt"`
}

const (
	// cacheTTL 是搜索结果的有效期，同 v2。
	cacheTTL = 24 * time.Hour
	// cacheLimit 是最多保留的搜索结果数，同 v2。
	cacheLimit = 50
)

// cacheKey 同 v2：群组、模式、参数。
func cacheKey(chatID, mode string, day int) string {
	return chatID + "_" + mode + "_" + strconv.Itoa(day)
}

// cache 是搜索结果的缓存：每个键一个文件，放在 data/clean_member/ 下，用到时才读。
//
// v2 把所有结果放在一个 JSON 里；照搬到 store.Store，50 份完整的成员名单会一直留在内存里，
// 每次读写还要整份深拷贝一遍——搜一个 5 万人的群就是好几 MB。本程序要的是常驻内存小，
// 所以拆成小文件，平时一份也不在内存里。MiBox 那个单文件的缓存不迁移：最多只能用一天。
type cache struct {
	dir string
}

// cacheKeyShape 是键的样子：只有数字和下划线，直接当文件名用，不必再做哈希；
// 文件名带着群组 ID，清掉一个群组的缓存时按前缀找就行。
var cacheKeyShape = regexp.MustCompile(`^\d+_[1-5]_\d+$`)

func (c cache) path(key string) (string, bool) {
	if !cacheKeyShape.MatchString(key) {
		return "", false
	}
	return filepath.Join(c.dir, key+".json"), true
}

// load 读一份还没过期的结果。
func (c cache) load(key string, now time.Time) (cacheData, bool) {
	path, ok := c.path(key)
	if !ok {
		return cacheData{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cacheData{}, false
	}
	var data cacheData
	if json.Unmarshal(raw, &data) != nil || data.ExpiresAt <= now.UnixMilli() {
		return cacheData{}, false
	}
	return data, true
}

// save 存一份结果，顺手清掉过期的文件；超过 50 份时丢掉最早写的，同 v2。
func (c cache) save(key string, data cacheData, now time.Time) error {
	path, ok := c.path(key)
	if !ok {
		return errors.New("invalid cache key " + key)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(path, raw, 0o600); err != nil {
		return err
	}
	return c.prune(now)
}

// prune 删掉过期的文件，剩下的多于 cacheLimit 份时删掉最早写的。按修改时间判断，
// 不用把每个文件读出来看 expiresAt：文件写下时就是搜索完成时，过期时间是它加 24 小时。
func (c cache) prune(now time.Time) error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	type file struct {
		path     string
		modified time.Time
	}
	var kept []file
	for _, entry := range entries {
		if entry.IsDir() || !cacheKeyShape.MatchString(strings.TrimSuffix(entry.Name(), ".json")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(c.dir, entry.Name())
		if now.Sub(info.ModTime()) >= cacheTTL {
			_ = os.Remove(path)
			continue
		}
		kept = append(kept, file{path: path, modified: info.ModTime()})
	}
	if len(kept) <= cacheLimit {
		return nil
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].modified.Before(kept[j].modified) })
	for _, old := range kept[:len(kept)-cacheLimit] {
		_ = os.Remove(old.path)
	}
	return nil
}

// clearChat 删掉一个群组的全部缓存。
func (c cache) clearChat(chatID string) error {
	paths, err := filepath.Glob(filepath.Join(c.dir, chatID+"_*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// isoTime 同 JS 的 Date.prototype.toISOString。
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// lastOnline 是报告里的「最后上线时间」：离线的写离线时刻，其余写状态名（同 v2 去掉
// UserStatus 前缀再转小写），没有状态时为 nil，报告里显示「未知」。
func lastOnline(status tg.UserStatusClass) *string {
	var value string
	switch s := status.(type) {
	case nil:
		return nil
	case *tg.UserStatusOffline:
		if s.WasOnline == 0 {
			value = "offline"
		} else {
			value = isoTime(time.Unix(int64(s.WasOnline), 0))
		}
	case *tg.UserStatusOnline:
		value = "online"
	case *tg.UserStatusRecently:
		value = "recently"
	case *tg.UserStatusLastWeek:
		value = "lastweek"
	case *tg.UserStatusLastMonth:
		value = "lastmonth"
	case *tg.UserStatusEmpty:
		value = "empty"
	default:
		return nil
	}
	return &value
}

// infoOf 把一个成员写成报告里的一行。
func infoOf(user *tg.User) userInfo {
	status, _ := user.GetStatus()
	return userInfo{ID: strconv.FormatInt(user.ID, 10), Username: user.Username, FirstName: user.FirstName,
		LastName: user.LastName, IsDeleted: user.Deleted, LastOnline: lastOnline(status)}
}

// formulaStart 是表格软件会当成公式的开头。v2 在这些值前面加一个单引号，防 CSV 注入：
// 成员可以把名字改成 =HYPERLINK(…)，打开报告的人一点就中招。
var formulaStart = regexp.MustCompile(`^[=+\-@\t\r]`)

func csvCell(value string) string {
	if formulaStart.MatchString(value) {
		value = "'" + value
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// csvReport 生成 CSV 报告，格式同 v2：开头带 BOM，Excel 才认得出 UTF-8；failed 为真时是失败名单，
// 多一列失败原因。
func csvReport(d cacheData, failed bool) []byte {
	heading := "群组清理报告"
	if failed {
		heading = "群组清理失败用户报告"
	}
	rows := [][]string{
		{heading},
		{"群组名称", d.ChatTitle},
		{"群组ID", d.ChatID},
		{"清理条件", modeName(d.Mode, d.Day)},
		{"搜索时间", d.SearchTime},
		{"符合条件用户数量", strconv.Itoa(d.TotalFound)},
		{},
	}
	header := []string{"用户ID", "用户名", "姓名", "最后上线时间", "是否注销"}
	if failed {
		header = append(header, "失败原因")
	}
	rows = append(rows, header)
	for _, user := range d.Users {
		online := "未知"
		if user.LastOnline != nil && *user.LastOnline != "" {
			online = *user.LastOnline
		}
		deleted := "否"
		if user.IsDeleted {
			deleted = "是"
		}
		row := []string{user.ID, user.Username, strings.TrimSpace(user.FirstName + " " + user.LastName), online, deleted}
		if failed {
			row = append(row, user.ErrorMessage)
		}
		rows = append(rows, row)
	}
	lines := make([]string, len(rows))
	for index, row := range rows {
		cells := make([]string, len(row))
		for column, value := range row {
			cells[column] = csvCell(value)
		}
		lines[index] = strings.Join(cells, ",")
	}
	return []byte("\ufeff" + strings.Join(lines, "\n"))
}

// reportName 是报告的文件名，同 v2 去掉了随机后缀：文件只发到收藏夹，不会在磁盘上撞名。
func reportName(d cacheData, failed bool, now time.Time) string {
	kind := "report"
	if failed {
		kind = "failed"
	}
	return kind + "_" + d.ChatID + "_" + d.Mode + "_" + strconv.Itoa(d.Day) + "_" + strconv.FormatInt(now.UnixMilli(), 10) + ".csv"
}
