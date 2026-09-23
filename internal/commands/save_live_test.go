package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// TestSaveLive 在真实账号的收藏夹里跑 .save 的两条路径：一条是转发，
// 另一条是受限对话逼出来的复制——媒体下载到磁盘，再带着原有属性重新
// 上传。跑完把自己发出的消息删掉。
//
// 复制是离线查不了的部分。测试用的消息所在对话允许转发，但复制并不
// 关心自己为什么被选中，所以跑的就是受保护频道会走的那套代码。
//
// 必须先停掉那个目录上的服务：同一个账号不能同时由两个进程服务。
//
//	MIBOT_SAVE_LIVE=/root/mibot-lite ./commands.test -test.run SaveLive -test.v
func TestSaveLive(t *testing.T) {
	root := os.Getenv("MIBOT_SAVE_LIVE")
	if root == "" {
		t.Skip("set MIBOT_SAVE_LIVE to a deployment directory whose service is stopped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	var failure error
	options := app.Options{Root: root, Version: "live-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AfterReady: func(ctx context.Context, a *app.App, client *bot.Client) error {
			failure = exerciseSave(ctx, t, client)
			return nil
		}}
	a, err := app.Prepare(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if failure != nil {
		t.Fatal(failure)
	}
}

func savedHistory(ctx context.Context, client *bot.Client, limit int) ([]*tg.Message, error) {
	result, err := client.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerSelf{}, Limit: limit})
	if err != nil {
		return nil, err
	}
	raw, _ := client.Unpack(result)
	var messages []*tg.Message
	for _, candidate := range raw {
		if message, ok := candidate.(*tg.Message); ok {
			messages = append(messages, message)
		}
	}
	return messages, nil
}

// looksLikeCommand 把任何可能被当成命令的消息排除在测试之外：复制一条
// ".bf"，就是新发一条内容为 ".bf" 的消息。
func looksLikeCommand(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, ".") || strings.HasPrefix(text, "。") || strings.HasPrefix(text, "$") || strings.HasPrefix(text, "!")
}

func attributeKinds(attributes []tg.DocumentAttributeClass) []string {
	kinds := make([]string, 0, len(attributes))
	for _, attribute := range attributes {
		kinds = append(kinds, fmt.Sprintf("%T", attribute))
	}
	return kinds
}

func exerciseSave(ctx context.Context, t *testing.T, client *bot.Client) error {
	history, err := savedHistory(ctx, client, 100)
	if err != nil {
		return err
	}
	var media, text *tg.Message
	for _, message := range history {
		if _, forwarded := message.GetFwdFrom(); forwarded || looksLikeCommand(message.Message) {
			continue
		}
		source, ok := bot.SourceOf(message)
		// 优先选文件（视频或语音消息带有复制时必须保留的属性），其次才是
		// 图片；大小要小到能很快跑完。
		if ok && source.Size < 30<<20 && (media == nil || (!source.Photo && isPhoto(media))) {
			media = message
		}
		if !ok && text == nil && strings.TrimSpace(message.Message) != "" {
			if _, hasMedia := message.GetMedia(); !hasMedia {
				text = message
			}
		}
	}
	if media == nil {
		return errors.New("Saved Messages has no photo or document under 30 MB to copy")
	}
	source, _ := bot.SourceOf(media)
	t.Logf("复制候选：#%d %s %s，属性 %v", media.ID, source.MimeType, formatBytes(int(source.Size)), attributeKinds(source.Attributes))

	scratch := t.TempDir()
	work := &saver{client: client, root: scratch, partial: filepath.Join(scratch, "partial"),
		upload: uploader.NewUploader(client.API()).WithThreads(4), progress: func(string) {}}
	var sent []int
	defer func() {
		if len(sent) > 0 {
			_, _ = client.API().MessagesDeleteMessages(context.WithoutCancel(ctx), &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: sent})
			t.Logf("已删除测试发出的 %d 条", len(sent))
		}
	}()
	newest := func() (*tg.Message, error) {
		latest, err := savedHistory(ctx, client, 1)
		if err != nil || len(latest) == 0 {
			return nil, fmt.Errorf("could not read back what was sent: %v", err)
		}
		return latest[0], nil
	}

	// 复制路径。
	started := time.Now()
	if err := work.copy(ctx, media, &tg.InputPeerSelf{}); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	copied, err := newest()
	if err != nil {
		return err
	}
	sent = append(sent, copied.ID)
	again, ok := bot.SourceOf(copied)
	if !ok {
		return errors.New("the copy arrived without media")
	}
	t.Logf("复制完成 %.1fs：#%d %s %s，属性 %v", time.Since(started).Seconds(), copied.ID, again.MimeType, formatBytes(int(again.Size)), attributeKinds(again.Attributes))
	if again.Photo != source.Photo {
		return errors.New("a photo came back as a document, or the other way round")
	}
	if !source.Photo {
		if again.MimeType != source.MimeType || again.Size != source.Size {
			return fmt.Errorf("the copy differs: %s %d bytes, want %s %d", again.MimeType, again.Size, source.MimeType, source.Size)
		}
		if !reflect.DeepEqual(attributeKinds(again.Attributes), attributeKinds(source.Attributes)) {
			return fmt.Errorf("attributes changed: %v, want %v", attributeKinds(again.Attributes), attributeKinds(source.Attributes))
		}
	}
	if copied.Message != media.Message {
		return fmt.Errorf("the caption changed: %q, want %q", copied.Message, media.Message)
	}

	// 转发路径。
	if text != nil {
		forwardedCopy, err := work.send(ctx, text, &tg.InputPeerSelf{}, &tg.InputPeerSelf{})
		if err != nil {
			return fmt.Errorf("forward: %w", err)
		}
		forwarded, err := newest()
		if err != nil {
			return err
		}
		if forwarded.ID <= copied.ID {
			return fmt.Errorf("nothing new arrived after the forward (newest is #%d)", forwarded.ID)
		}
		sent = append(sent, forwarded.ID)
		header, isForward := forwarded.GetFwdFrom()
		t.Logf("转发：#%d → #%d，代码判定复制=%v，Telegram 附转发头=%v %+v，noforwards=%v",
			text.ID, forwarded.ID, forwardedCopy, isForward, header.FromID, text.Noforwards)
		// 只断言 .save 自己做的决定。Telegram 会不会给自己转给自己的消息
		// 加转发头，不在断言范围内。
		if forwardedCopy {
			return errors.New("an ordinary message was copied instead of forwarded")
		}
	}

	// 本地模式：文件完整落盘，旁边带着元数据。
	path, err := work.saveLocal(ctx, media, messageLink{ChatID: "self", ID: media.ID})
	if err != nil {
		return fmt.Errorf("local: %w", err)
	}
	info, err := os.Stat(filepath.Join(scratch, path))
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(scratch, path) + ".json"); err != nil {
		return errors.New("the local save has no metadata beside it")
	}
	if !source.Photo && info.Size() != source.Size {
		return fmt.Errorf("local file is %d bytes, want %d", info.Size(), source.Size)
	}
	if leftovers, _ := os.ReadDir(filepath.Join(scratch, "partial")); len(leftovers) != 0 {
		return fmt.Errorf("%d partial files were left behind", len(leftovers))
	}
	t.Logf("本地保存：%s（%s）", path, formatBytes(int(info.Size())))
	return nil
}

func isPhoto(message *tg.Message) bool {
	source, ok := bot.SourceOf(message)
	return ok && source.Photo
}
