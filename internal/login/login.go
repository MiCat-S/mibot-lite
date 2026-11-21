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

	gotdsession "github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/config"
	"github.com/MiCat-S/mibot-lite/internal/fsutil"
	"github.com/MiCat-S/mibot-lite/internal/session"
)

// Options 是一次登录的配置。
type Options struct {
	Root    string
	APIID   int
	APIHash string
	Force   bool
	In      *os.File
	Out     io.Writer
}

// Run 登录并写出文件。Telegram 确认账号之前不改账号配置。
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
	lock, err := app.LockRoot(root)
	if errors.Is(err, app.ErrRunning) {
		return errors.New("a mibot-lite instance is running on this directory; stop it before signing in")
	}
	if err != nil {
		return err
	}
	defer lock.Close()

	existing := map[string]any{}
	var proxy *config.Proxy
	if raw, err := os.ReadFile(filepath.Join(root, config.ConfigFile)); err == nil {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&existing); err != nil {
			return fmt.Errorf("config.json: %w", err)
		}
		if existing == nil {
			return errors.New("config.json must contain an object")
		}
		current, _ := existing["session"].(string)
		if current != "" && !options.Force {
			return errors.New("config.json already holds a session; pass --force to replace it")
		}
		proxy, err = config.ParseProxy(raw)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
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
	resolver, err := app.TelegramResolver(proxy)
	if err != nil {
		return err
	}
	client := telegram.NewClient(apiID, apiHash, telegram.Options{SessionStorage: memory, Device: telegram.DeviceConfig{DeviceModel: device}, Resolver: resolver})
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
	if err := writeAccount(ctx, root, append(document, '\n'), data); err != nil {
		return err
	}
	fmt.Fprintf(out, "Signed in as %s. config.json and %s written to %s\n", who, config.SessionFile, root)
	return nil
}

func writeAccount(ctx context.Context, root string, document []byte, data *gotdsession.Data) error {
	staging, err := os.MkdirTemp(root, ".login-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	stagedConfig := filepath.Join(staging, config.ConfigFile)
	if err := fsutil.WriteFileAtomic(stagedConfig, document, 0o600); err != nil {
		return err
	}
	file := session.FileStorage{Path: filepath.Join(staging, config.SessionFile)}
	if err := (&gotdsession.Loader{Storage: file}).Save(ctx, data); err != nil {
		return fmt.Errorf("write %s: %w", config.SessionFile, err)
	}
	// 启动时可以从 config.json 导入缺失的 gotd 会话。先撤下缓存，再原子切换配置，
	// 中途退出时仍有一份完整配置可供启动，不会混用两个账号。
	for _, name := range []string{config.StateFile, config.StateFile + ".cursor", config.RestartReceiptFile, config.SessionFile} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(stagedConfig, filepath.Join(root, config.ConfigFile)); err != nil {
		return err
	}
	return os.Rename(file.Path, filepath.Join(root, config.SessionFile))
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
