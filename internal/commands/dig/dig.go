// Package dig 实现 .dig：调用主机上的 dig 查 DNS 记录，给结果里的 IP 附上归属地和 AS 号。
//
// dig 直接用 os/exec 启动，不经过 shell；参数逐个校验，只放行几种查询类型和几个输出选项，
// 用户给的东西不会被 dig 当成别的选项。
package dig

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

const (
	// runTimeout 是一次 dig 的总时限，和 MiBox 一样 10 秒。dig 自己的重试另外压到
	// 3 秒 × 2 次（见 buildArgs），服务器不应答时它先报「没有应答」，不至于被硬杀掉。
	runTimeout = 10 * time.Second
	// maxOutput 是保留的 dig 输出上限，和 MiBox 一样 32 KB；再多的截掉。
	maxOutput = 32 << 10
)

// recordTypes 是允许查的记录类型，和 MiBox 一样。
var recordTypes = []string{"A", "AAAA", "MX", "CNAME", "TXT", "NS", "SOA", "PTR", "SRV", "CAA"}

// outputFlags 是允许的输出选项，和 MiBox 一样；别的 + 选项（比如 +trace、+time）一律不收。
var outputFlags = []string{"+short", "+noall", "+answer", "+stats", "+comments", "+tcp"}

var (
	// domainPattern 是要查的名字：允许下划线（_dmarc、_sip._tcp 这类 SRV/TXT 名字），
	// 顶级域至少两个字母或是 xn-- 开头的国际化顶级域（.中国），可以带末尾的点。
	domainPattern = regexp.MustCompile(`^(?:[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.)+(?:[A-Za-z]{2,63}|xn--[A-Za-z0-9-]{1,59})\.?$`)
	// serverPattern 是用主机名指定的 DNS 服务器，不允许下划线。
	serverPattern = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+(?:[A-Za-z]{2,63}|xn--[A-Za-z0-9-]{1,59})\.?$`)
)

// query 是校验过的一次查询。
type query struct {
	// name 是要查的域名（已转成 ASCII）；reverse 为真时它是要反查的 IP。
	name    string
	reverse bool
	kind    string
	server  string
	flags   []string
}

// parseArgs 校验参数：域名 [类型] [服务器|@服务器] [+选项…]。出错时返回给用户看的错误。
//
// 和 MiBox 相比多了两点：第一个参数是 IP 时做反查（dig -x），不用自己拼 in-addr.arpa；
// 中文等国际化域名先转成 xn-- 形式再查。
func parseArgs(input []string) (query, error) {
	var flags, servers, rest []string
	for _, value := range input {
		switch {
		case strings.HasPrefix(value, "+"):
			flag := strings.ToLower(value)
			if !slices.Contains(outputFlags, flag) {
				return query{}, kit.Failf("不支持的选项 %s，只能用 %s", value, strings.Join(outputFlags, " "))
			}
			if !slices.Contains(flags, flag) {
				flags = append(flags, flag)
			}
		case strings.HasPrefix(value, "@"):
			servers = append(servers, value[1:])
		default:
			rest = append(rest, value)
		}
	}
	if len(servers) > 1 {
		return query{}, kit.Fail("只能指定一个 DNS 服务器")
	}
	if len(rest) == 0 {
		return query{}, kit.Fail("请写上要查的域名")
	}
	if len(rest) > 3 || (len(servers) == 1 && len(rest) > 2) {
		return query{}, kit.Fail("参数过多")
	}
	q := query{kind: "A", flags: flags}
	if len(rest) > 1 {
		q.kind = strings.ToUpper(rest[1])
		if !slices.Contains(recordTypes, q.kind) {
			return query{}, kit.Failf("不支持的记录类型 %s，可用 %s", rest[1], strings.Join(recordTypes, " "))
		}
	}
	if ip, ok := parseIP(rest[0]); ok {
		if len(rest) > 1 && q.kind != "PTR" {
			return query{}, kit.Fail("IP 只能反查 PTR 记录")
		}
		q.name, q.kind, q.reverse = ip, "PTR", true
	} else {
		name, err := asciiDomain(rest[0])
		if err != nil {
			return query{}, err
		}
		q.name = name
	}
	switch {
	case len(servers) == 1:
		q.server = servers[0]
		if q.server == "" {
			return query{}, kit.Fail("DNS 服务器格式无效")
		}
	case len(rest) == 3:
		q.server = rest[2]
	}
	if q.server != "" && !validServer(q.server) {
		return query{}, kit.Fail("DNS 服务器格式无效，写 IP 或主机名")
	}
	return q, nil
}

// asciiDomain 把域名转成 dig 认的 ASCII 形式并校验。
func asciiDomain(value string) (string, error) {
	name := value
	if !isASCII(name) {
		converted, err := idna.Lookup.ToASCII(name)
		if err != nil {
			return "", kit.Fail("域名格式无效")
		}
		name = converted
	}
	if len(name) > 253 || !domainPattern.MatchString(name) {
		return "", kit.Fail("域名格式无效")
	}
	return name, nil
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

func validServer(server string) bool {
	if _, ok := parseIP(server); ok {
		return true
	}
	return len(server) <= 253 && serverPattern.MatchString(server)
}

// buildArgs 是交给 dig 的参数。每个值都校验过：域名和服务器不会以 - 开头，
// 服务器前面固定加 @，所以不会被 dig 当成选项。
func buildArgs(q query) []string {
	var args []string
	if q.server != "" {
		args = append(args, "@"+q.server)
	}
	if q.reverse {
		args = append(args, "-x", q.name)
	} else {
		args = append(args, q.name, q.kind)
	}
	flags := q.flags
	if len(flags) == 0 {
		flags = []string{"+short"}
	}
	args = append(args, flags...)
	// 压短 dig 自己的等待：默认 5 秒 × 3 次会超过 runTimeout，被杀掉的 dig 什么也不输出。
	return append(args, "+time=3", "+tries=2")
}

// borrowedServerAllowed 判断借用（.sudo、.sure）时指定的 DNS 服务器能不能用。
// 借用的人只能指定公网 IP：内网地址和主机名（解析出来可能是内网）会让别人借本机去探内网的 DNS。
func borrowedServerAllowed(server string) bool {
	if server == "" {
		return true
	}
	ip, ok := parseIP(server)
	return ok && publicIP(ip)
}

// runner 执行 dig，返回标准输出和输出是否被截断。测试里换成假的。
var runner = runDig

// binary 找 dig：生产主机上是 /usr/bin/dig（MiBox 也固定用这个），开发机上从 PATH 找。
func binary() (string, error) {
	if _, err := os.Stat("/usr/bin/dig"); err == nil {
		return "/usr/bin/dig", nil
	}
	return exec.LookPath("dig")
}

func runDig(ctx context.Context, args []string) ([]byte, bool, error) {
	path, err := binary()
	if err != nil {
		return nil, false, errNoDig
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	tool := exec.CommandContext(ctx, path, args...)
	// 不继承进程的环境变量（里面可能有 MIBOT_* 的 Key）；LC_ALL=C 让输出不随语言变。
	tool.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	tool.WaitDelay = time.Second
	stdout := &capped{limit: maxOutput}
	stderr := &capped{limit: 4 << 10}
	tool.Stdout, tool.Stderr = stdout, stderr
	err = tool.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, false, errTimeout
	}
	if err != nil {
		return stdout.buf.Bytes(), stdout.truncated, &digError{cause: err, stderr: strings.TrimSpace(stderr.buf.String()), stdout: stdout.buf.String()}
	}
	return stdout.buf.Bytes(), stdout.truncated, nil
}

var (
	errNoDig   = errors.New("dig not found")
	errTimeout = errors.New("dig timed out")
)

// digError 是 dig 以非零状态退出。stderr、stdout 只进日志，不进聊天（是英文，还可能带路径）。
type digError struct {
	cause          error
	stderr, stdout string
}

func (e *digError) Error() string {
	return "dig: " + e.cause.Error() + ": " + kit.TruncateRunes(e.stderr+" "+e.stdout, 300)
}

func (e *digError) Unwrap() error { return e.cause }

// explain 把运行失败换成给用户看的话。
func explain(err error) error {
	var failure *digError
	var exit *exec.ExitError
	switch {
	case errors.Is(err, errNoDig):
		return kit.Fail("主机上没有 dig，装上 dnsutils（Debian/Ubuntu）或 bind-utils 后再试")
	case errors.Is(err, errTimeout):
		return kit.Fail("查询超时，稍后再试或换一个 DNS 服务器")
	case errors.As(err, &failure) && errors.As(failure.cause, &exit) && exit.ExitCode() == 9:
		// dig 的退出码 9 是服务器没有应答。
		return kit.Fail("DNS 服务器没有应答，稍后再试或换一个服务器")
	case errors.As(err, &failure) && strings.Contains(failure.stderr+failure.stdout, "couldn't get address"):
		return kit.Fail("解析不了指定的 DNS 服务器")
	}
	return kit.FailWith("DNS 查询失败", err)
}

// capped 是只留前 limit 字节的输出缓冲，多出来的丢掉但照样报告写成功，
// 免得 dig 因为管道写不进去而出错。
type capped struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = c.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// trimOutput 去掉截断时留下的半行（可能是半个 UTF-8 字符）。
func trimOutput(output []byte, truncated bool) string {
	text := string(output)
	if truncated {
		if index := strings.LastIndexByte(text, '\n'); index >= 0 {
			text = text[:index]
		}
	}
	return strings.TrimRight(strings.ToValidUTF8(text, ""), "\n\r\t ")
}

// render 拼出结果：标题、查询条件，dig 的输出放在 <pre> 里，每个 IP 下面一行归属地。
func render(q query, output string, locations map[string]string, truncated bool) string {
	head := "🧭 <b>DNS 查询</b>\n" + command.Code(q.name) + " · " + command.Code(q.kind)
	if q.server != "" {
		head += " · " + command.Code("@"+q.server)
	}
	if output == "" {
		return head + "\n\n没有记录"
	}
	body := command.Escape(annotate(output, locations))
	text := head + "\n\n<pre>" + body + "</pre>"
	if truncated {
		text += "\n⚠️ 输出超过 32 KB，后面的省略了"
	}
	return text
}

func help(prefix string) string {
	p := command.Escape(prefix)
	c := func(text string) string { return "<code>" + p + text + "</code>" }
	return "🧭 <b>DNS 查询</b>\n\n用主机上的 dig 查 DNS 记录，结果里的 IP 附上归属地和 AS 号。\n\n" +
		"• " + c("dig example.com") + " 查 A 记录\n" +
		"• " + c("dig example.com MX") + " 指定记录类型\n" +
		"• " + c("dig example.com MX @1.1.1.1") + " 指定 DNS 服务器，也可以写成第三个参数\n" +
		"• " + c("dig example.com MX +noall +answer") + " 指定输出选项\n" +
		"• " + c("dig 8.8.8.8") + " 反查 IP 的 PTR 记录\n\n" +
		"<b>参数</b>\n" +
		"• 记录类型：" + strings.Join(recordTypes, "、") + "\n" +
		"• 输出选项：" + command.Code(strings.Join(outputFlags, " ")) + "，默认 " + command.Code("+short") + "\n" +
		"• DNS 服务器写 IP 或主机名；借用时只能写公网 IP\n\n" +
		"归属地来自 ip-api.com。主机上要装有 dig（dnsutils 或 bind-utils）。"
}

// Register 注册 .dig。不存东西，每次现查。
func Register(a *app.App) {
	a.Registry.Register(&command.Command{Name: "dig", Group: command.GroupTools, Description: "查询 DNS 记录",
		Usage: "域名 [类型] [@服务器] [+选项…]", Help: help, Timeout: time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			if len(inv.Args) == 0 {
				return inv.Edit(ctx, help(inv.Prefix))
			}
			q, err := parseArgs(inv.Args)
			if err != nil {
				return err
			}
			if inv.Trigger != nil && !borrowedServerAllowed(q.server) {
				return kit.Fail("借用时只能用公网 IP 指定 DNS 服务器")
			}
			if err := inv.EditText(ctx, kit.Working("正在查询 "+q.name)); err != nil {
				return err
			}
			text, err := lookup(ctx, inv.Log, q)
			if err != nil {
				return err
			}
			return inv.EditPages(ctx, command.HTMLPages(text, command.PageLimit))
		}})
}

// lookup 跑一次 dig、查归属地，返回拼好的结果（HTML，还没分页）。
func lookup(ctx context.Context, logger *slog.Logger, q query) (string, error) {
	output, truncated, err := runner(ctx, buildArgs(q))
	if err != nil {
		return "", explain(err)
	}
	text := trimOutput(output, truncated)
	locations, err := locate(ctx, addresses(text))
	if err != nil && logger != nil {
		// 归属地只是附带的，查不到也照样给出 DNS 结果。
		logger.Warn("dig.locate_failed", "error", err.Error())
	}
	return render(q, text, locations, truncated), nil
}
