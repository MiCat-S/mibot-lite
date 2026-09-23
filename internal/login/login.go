// Package login 在终端里登录 Telegram 账号，写出 config.json（gramjs
// 会话，也就是 MiBox 读取的格式）和 gotd 自己的会话文件，之后两种
// 运行时都能使用这个账号。
package login

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	gotdsession "github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"

	"github.com/MiCat-S/mibot-lite/internal/config"
	"github.com/MiCat-S/mibot-lite/internal/session"
)

// SessionFile 是 gotd 会话文件的文件名，与 MiBox 的 Go 宿主共用。
const SessionFile = "gotd-session.json"

// lockFile 是运行中的服务持有的实例锁，名字与 internal/app 保持一致。
const lockFile = "mibot-lite.lock"

// Options 是一次登录的配置。
type Options struct {
	Root    string
	APIID   int
	APIHash string
	Force   bool
	In      *os.File
	Out     io.Writer
}

// Run 登录并写出文件。Telegram 确认账号之前，磁盘上什么都不会改。
func Run(ctx context.Context, options Options) error {
	out := options.Out
	if out == nil {
		out = os.Stdout
	}
	in := options.In
	if in == nil {
		in = os.Stdin
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("deployment directory %s does not exist; create it first", root)
	}
	existing := map[string]any{}
	if raw, err := os.ReadFile(filepath.Join(root, "config.json")); err == nil {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&existing); err != nil {
			return fmt.Errorf("config.json: %w", err)
		}
		current, _ := existing["session"].(string)
		switch {
		case current != "" && !options.Force:
			return errors.New("config.json already holds a session; pass --force to replace it")
		case current != "":
			// --force 的意思是「替换会话」，不是「在有进程正用着它时
			// 替换」。那样的话，服务会继续用一个已经不是磁盘上那份的
			// 会话运行，下次重启就会悄悄变成另一个登录。
			if err := refuseWhileRunning(root); err != nil {
				return err
			}
		}
	}
	prompt := &terminal{in: in, out: out, reader: bufio.NewReader(in)}
	apiID, apiHash := options.APIID, options.APIHash
	if apiID == 0 {
		if number, ok := existing["api_id"].(json.Number); ok {
			if parsed, err := number.Int64(); err == nil {
				apiID = int(parsed)
			}
		}
	}
	if apiHash == "" {
		apiHash, _ = existing["api_hash"].(string)
	}
	if apiID == 0 {
		text, err := prompt.line("API ID: ")
		if err != nil {
			return err
		}
		apiID, err = strconv.Atoi(text)
		if err != nil || apiID <= 0 {
			return errors.New("API ID must be a positive number")
		}
	}
	if apiHash == "" {
		text, err := prompt.line("API hash: ")
		if err != nil {
			return err
		}
		if apiHash = strings.TrimSpace(text); apiHash == "" {
			return errors.New("API hash is required")
		}
	}

	memory := &gotdsession.StorageMemory{}
	device := config.DefaultDeviceModel
	if name, ok := existing["app_name"].(string); ok && strings.TrimSpace(name) != "" {
		device = name
	}
	client := telegram.NewClient(apiID, apiHash, telegram.Options{SessionStorage: memory, Device: telegram.DeviceConfig{DeviceModel: device}})
	var who string
	err = client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(authenticator{prompt: prompt}, auth.SendCodeOptions{})
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return err
		}
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !status.Authorized || status.User == nil {
			return errors.New("sign-in completed but the session is not authorized")
		}
		who = fmt.Sprintf("id %d", status.User.ID)
		if status.User.Username != "" {
			who = "@" + status.User.Username + " (" + who + ")"
		}
		return nil
	})
	if err != nil {
		return err
	}
	data, err := (&gotdsession.Loader{Storage: memory}).Load(ctx)
	if err != nil {
		return fmt.Errorf("read the new session: %w", err)
	}
	parsed, err := session.FromData(data)
	if err != nil {
		return err
	}
	encoded, err := parsed.Encode()
	if err != nil {
		return err
	}
	existing["api_id"] = json.Number(strconv.Itoa(apiID))
	existing["api_hash"] = apiHash
	existing["session"] = encoded
	document, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(root, "config.json"), append(document, '\n')); err != nil {
		return err
	}
	file := &gotdsession.FileStorage{Path: filepath.Join(root, SessionFile)}
	if err := (&gotdsession.Loader{Storage: file}).Save(ctx, data); err != nil {
		return fmt.Errorf("write %s: %w", SessionFile, err)
	}
	_ = os.Chmod(file.Path, 0o600)
	fmt.Fprintf(out, "Signed in as %s. config.json and %s written to %s\n", who, SessionFile, root)
	return nil
}

// refuseWhileRunning 在已有进程持有这个部署的实例锁时返回错误。
//
// 这个锁和运行中的服务拿的是同一个 flock，所以这里直接问内核，而不是
// 根据 pid 文件或服务单元名去猜：不管部署是跑在 systemd 下、终端里，
// 还是根本没在跑，结果都是对的。
func refuseWhileRunning(root string) error {
	path := filepath.Join(root, lockFile)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// 锁文件连打开都打不开，并不能说明有东西在运行；如果目录
		// 不可用，登录后面自然会因为别的原因失败。
		return nil
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("a mibot-lite instance is running on this directory; stop it before signing in again")
	}
	// 马上释放：登录不需要一直拿着锁，只需要知道有没有别人拿着。
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return nil
}

func writeFile(path string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config.json.")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		os.Remove(name)
		return err
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		os.Remove(name)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

type terminal struct {
	in     *os.File
	out    io.Writer
	reader *bufio.Reader
}

func (t *terminal) line(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	line, err := t.reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (t *terminal) secret(prompt string) (string, error) {
	fd := int(t.in.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprintln(t.out, "(input is not a terminal; the next value will not be hidden)")
		return t.line(prompt)
	}
	fmt.Fprint(t.out, prompt)
	secret, err := term.ReadPassword(fd)
	fmt.Fprintln(t.out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(secret)), nil
}

type authenticator struct{ prompt *terminal }

func (a authenticator) Phone(context.Context) (string, error) {
	return a.prompt.line("Phone number (+country code): ")
}

func (a authenticator) Password(context.Context) (string, error) {
	return a.prompt.secret("Two-step verification password: ")
}

func (a authenticator) Code(_ context.Context, sent *tg.AuthSentCode) (string, error) {
	where := "the code Telegram sent you"
	if sent != nil {
		switch sent.Type.(type) {
		case *tg.AuthSentCodeTypeApp:
			where = "the code from your other Telegram app"
		case *tg.AuthSentCodeTypeSMS:
			where = "the code from the SMS"
		case *tg.AuthSentCodeTypeCall:
			where = "the code from the phone call"
		}
	}
	return a.prompt.line("Telegram login code (" + where + "): ")
}

var errNoAccount = errors.New("this phone number has no Telegram account; create one in the official app first")

func (authenticator) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errNoAccount
}

func (authenticator) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errNoAccount
}
