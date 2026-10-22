package dme

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// fakeTelegram 冒充 Telegram：数上传了几个分片，记下每次编辑换上的媒体，
// 编辑成功时像服务器一样把换上去的照片放在应答里。
type fakeTelegram struct {
	mu    sync.Mutex
	parts int
	media []tg.InputMediaClass
	// refuse 是引用已有照片编辑时要回的错误，回一次就清空。
	refuse error
}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (f *fakeTelegram) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch request := input.(type) {
	case *tg.UploadSaveFilePartRequest:
		f.parts++
		return respond(output, &tg.BoolTrue{})
	case *tg.MessagesEditMessageRequest:
		f.media = append(f.media, request.Media)
		if _, reused := request.Media.(*tg.InputMediaPhoto); reused && f.refuse != nil {
			err := f.refuse
			f.refuse = nil
			return err
		}
		photo := &tg.Photo{ID: 77, AccessHash: 5, FileReference: []byte{1}}
		return respond(output, &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateEditMessage{
			Message: &tg.Message{ID: request.ID, PeerID: &tg.PeerUser{UserID: 1}, Media: &tg.MessageMediaPhoto{Photo: photo}},
		}}})
	}
	return nil
}

func newScrubRun(fake *fakeTelegram) *dmeRun {
	client := bot.FromAPI(tg.NewClient(fake), bot.NewPeerCache(), &tg.User{ID: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &dmeRun{ctx: context.Background(), client: client, api: client.API(), peer: &tg.InputPeerSelf{}}
}

func photoMessage(id int) *tg.Message {
	return &tg.Message{ID: id, Date: int(time.Now().Unix()), Media: &tg.MessageMediaPhoto{}}
}

// 占位图只上传一次，后面的消息引用第一次换上去的那张照片。
func TestScrubUploadsPlaceholderOnce(t *testing.T) {
	fake := &fakeTelegram{}
	run := newScrubRun(fake)
	for id := 1; id <= 3; id++ {
		run.scrub(photoMessage(id))
	}
	if fake.parts != 1 {
		t.Errorf("uploaded %d parts, want 1", fake.parts)
	}
	if run.edited != 3 || run.failedEdits != 0 {
		t.Errorf("edited %d, failed %d; want 3, 0", run.edited, run.failedEdits)
	}
	if _, uploaded := fake.media[0].(*tg.InputMediaUploadedPhoto); !uploaded {
		t.Errorf("first edit used %T, want an upload", fake.media[0])
	}
	for _, media := range fake.media[1:] {
		reused, ok := media.(*tg.InputMediaPhoto)
		if !ok {
			t.Fatalf("later edit used %T, want the stored photo", media)
		}
		if photo, ok := reused.ID.(*tg.InputPhoto); !ok || photo.ID != 77 || photo.AccessHash != 5 {
			t.Errorf("reused photo = %+v, want id 77 hash 5", reused.ID)
		}
	}
}

// file reference 过期时这一条重新上传，照样换成功。
func TestScrubReuploadsWhenReferenceExpires(t *testing.T) {
	fake := &fakeTelegram{}
	run := newScrubRun(fake)
	run.scrub(photoMessage(1))
	fake.refuse = tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	run.scrub(photoMessage(2))
	if fake.parts != 2 {
		t.Errorf("uploaded %d parts, want 2", fake.parts)
	}
	if run.edited != 2 || run.failedEdits != 0 {
		t.Errorf("edited %d, failed %d; want 2, 0", run.edited, run.failedEdits)
	}
	if run.placeholder == nil {
		t.Error("the re-uploaded photo was not kept")
	}
}

// 别的编辑失败（过了编辑时限、没有权限）只记失败，不为此重新上传，存下的照片也照旧用。
func TestScrubKeepsPhotoOnOtherFailures(t *testing.T) {
	fake := &fakeTelegram{}
	run := newScrubRun(fake)
	run.scrub(photoMessage(1))
	fake.refuse = tgerr.New(400, "MESSAGE_EDIT_TIME_EXPIRED")
	run.scrub(photoMessage(2))
	run.scrub(photoMessage(3))
	if fake.parts != 1 {
		t.Errorf("uploaded %d parts, want 1", fake.parts)
	}
	if run.edited != 2 || run.failedEdits != 1 {
		t.Errorf("edited %d, failed %d; want 2, 1", run.edited, run.failedEdits)
	}
}
