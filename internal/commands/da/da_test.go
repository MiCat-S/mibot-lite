package da

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// permissionFake 是只回答两个权限接口的假 Telegram：查自己的成员身份按 participantErr
// 失败或返回普通成员，管理员列表按 admins 返回（adminsErr 不为空时失败）。
type permissionFake struct {
	participantErr error
	admins         []tg.ChannelParticipantClass
	adminsErr      error
	calls          []string
}

func (p *permissionFake) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	var value bin.Encoder
	switch request := input.(type) {
	case *tg.ChannelsGetParticipantRequest:
		p.calls = append(p.calls, "participant")
		if p.participantErr != nil {
			return p.participantErr
		}
		value = &tg.ChannelsChannelParticipant{Participant: &tg.ChannelParticipantSelf{UserID: 1, InviterID: 2, Date: 1}}
	case *tg.ChannelsGetParticipantsRequest:
		p.calls = append(p.calls, fmt.Sprintf("participants %s limit=%d", request.Filter.TypeName(), request.Limit))
		if p.adminsErr != nil {
			return p.adminsErr
		}
		value = &tg.ChannelsChannelParticipants{Count: len(p.admins), Participants: p.admins}
	default:
		return fmt.Errorf("假 Telegram 不认识 %T", input)
	}
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func permissionRun(fake *permissionFake) *daRun {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := bot.FromAPI(tg.NewClient(fake), bot.NewPeerCache(), &tg.User{ID: 1, Self: true}, logger)
	return &daRun{ctx: context.Background(), client: client, api: client.API(),
		peer: &tg.InputPeerChannel{ChannelID: 10, AccessHash: 1}, logger: logger}
}

// TestIsAdminFallsBackToAdminList 检查查自己的成员身份失败时改查管理员列表，和原版一样。
func TestIsAdminFallsBackToAdminList(t *testing.T) {
	failed := tgerr.New(400, "CHANNEL_PRIVATE")
	for name, tc := range map[string]struct {
		fake  *permissionFake
		want  bool
		calls []string
	}{
		"直接查到普通成员": {fake: &permissionFake{}, want: false, calls: []string{"participant"}},
		"列表里有自己": {fake: &permissionFake{participantErr: failed, admins: []tg.ChannelParticipantClass{
			&tg.ChannelParticipantCreator{UserID: 9},
			&tg.ChannelParticipantAdmin{UserID: 1, PromotedBy: 9, Date: 1},
		}}, want: true, calls: []string{"participant", "participants channelParticipantsAdmins limit=100"}},
		"列表里只有提拔别人的自己": {fake: &permissionFake{participantErr: failed, admins: []tg.ChannelParticipantClass{
			&tg.ChannelParticipantAdmin{UserID: 5, PromotedBy: 1, Date: 1},
		}}, want: false, calls: []string{"participant", "participants channelParticipantsAdmins limit=100"}},
		"两样都失败": {fake: &permissionFake{participantErr: failed, adminsErr: failed}, want: false,
			calls: []string{"participant", "participants channelParticipantsAdmins limit=100"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := permissionRun(tc.fake).isAdmin(); got != tc.want {
				t.Fatalf("isAdmin = %v，应为 %v", got, tc.want)
			}
			if !slices.Equal(tc.fake.calls, tc.calls) {
				t.Fatalf("请求 %v，应为 %v", tc.fake.calls, tc.calls)
			}
		})
	}
}
