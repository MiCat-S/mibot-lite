package save

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestLostAlbumResponseDoesNotReplay(t *testing.T) {
	var requests []*tg.MessagesForwardMessagesRequest
	fake := telegram.InvokeFunc(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		req := in.(*tg.MessagesForwardMessagesRequest)
		requests = append(requests, req)
		// 服务端已经接收相册，响应却丢了；后续发送都能成功。
		if len(requests) == 1 {
			return io.ErrUnexpectedEOF
		}
		out.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: 100 + len(requests)}
		return nil
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	from, to := &tg.InputPeerSelf{}, &tg.InputPeerSelf{}
	work := &saver{client: client, progress: func(string) {}}
	jobs := []saveJob{
		{album: true, peer: from, messages: []*tg.Message{{ID: 10, PeerID: &tg.PeerUser{UserID: 1}}, {ID: 11, PeerID: &tg.PeerUser{UserID: 1}}}},
		{peer: from, messages: []*tg.Message{{ID: 12, PeerID: &tg.PeerUser{UserID: 1}}}},
	}
	tally, err := runSaveJobs(context.Background(), work, jobs, to, false)
	if err != nil || len(requests) != 2 || tally.saved != 1 || len(tally.failures) != 1 {
		t.Fatalf("album replayed or later job dropped after unknown result: requests=%d %+v err=%v", len(requests), tally, err)
	}
	if ids := requests[1].ID; len(ids) != 1 || ids[0] != 12 {
		t.Fatalf("album was sent again: %v", ids)
	}
}

// 明确被拒时什么都没有发出，逐条再试不会重复，能转的照常保存。
func TestRejectedAlbumFallsBackToSingleForwards(t *testing.T) {
	var requests []*tg.MessagesForwardMessagesRequest
	fake := telegram.InvokeFunc(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		req := in.(*tg.MessagesForwardMessagesRequest)
		requests = append(requests, req)
		if len(req.ID) > 1 || req.ID[0] == 10 {
			return tgerr.New(400, "MESSAGE_ID_INVALID")
		}
		out.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: 100 + len(requests)}
		return nil
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	work := &saver{client: client, progress: func(string) {}}
	job := saveJob{album: true, peer: &tg.InputPeerSelf{}, messages: []*tg.Message{{ID: 10, PeerID: &tg.PeerUser{UserID: 1}}, {ID: 11, PeerID: &tg.PeerUser{UserID: 1}}}}
	tally, err := runSaveJobs(context.Background(), work, []saveJob{job}, &tg.InputPeerSelf{}, false)
	if err != nil || len(requests) != 3 || tally.saved != 1 || len(tally.failures) != 1 {
		t.Fatalf("rejected album fallback: requests=%d %+v err=%v", len(requests), tally, err)
	}
}

func TestRestrictedAlbumStillCopiesMessages(t *testing.T) {
	var texts []string
	fake := telegram.InvokeFunc(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.MessagesForwardMessagesRequest:
			return tgerr.New(400, "CHAT_FORWARDS_RESTRICTED")
		case *tg.MessagesSendMessageRequest:
			texts = append(texts, req.Message)
			out.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: 100 + len(texts)}
			return nil
		default:
			t.Errorf("unexpected %T", in)
			return errors.New("unexpected RPC")
		}
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	work := &saver{client: client, progress: func(string) {}}
	messages := []*tg.Message{{ID: 10, Message: "first", PeerID: &tg.PeerUser{UserID: 1}}, {ID: 11, Message: "second", PeerID: &tg.PeerUser{UserID: 1}}}
	tally, err := runSaveJobs(context.Background(), work, []saveJob{{album: true, peer: &tg.InputPeerSelf{}, messages: messages}}, &tg.InputPeerSelf{}, false)
	if err != nil || tally.saved != 2 || tally.copied != 2 || len(texts) != 2 || texts[0] != "first" || texts[1] != "second" {
		t.Fatalf("restriction copy fallback: %+v %v %v", tally, texts, err)
	}
}
