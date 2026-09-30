package bot

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// 发文字只回 updateShortSentMessage 时，对话从请求里取；发媒体、转发回的 Updates 里
// 每条新消息都要记下；编辑的不记。
func TestSentRecorder(t *testing.T) {
	type mark struct {
		chat string
		id   int
	}
	var marks []mark
	recorder := SentRecorder{Self: func() int64 { return 1 }, Mark: func(peer tg.PeerClass, id int) {
		marks = append(marks, mark{PeerID(peer), id})
	}}
	channelMessage := &tg.Message{ID: 30, PeerID: &tg.PeerChannel{ChannelID: 9}}
	edited := &tg.Message{ID: 31, PeerID: &tg.PeerChannel{ChannelID: 9}}
	for name, c := range map[string]struct {
		reply bin.Encoder
		call  func(*tg.Client) error
		want  []mark
	}{
		"收藏夹里发文字": {
			reply: &tg.UpdateShortSentMessage{ID: 10},
			call: func(api *tg.Client) error {
				_, err := api.MessagesSendMessage(context.Background(), &tg.MessagesSendMessageRequest{Peer: &tg.InputPeerSelf{}, Message: ".dme 1"})
				return err
			},
			want: []mark{{"1", 10}},
		},
		"私聊里发文字": {
			reply: &tg.UpdateShortSentMessage{ID: 11},
			call: func(api *tg.Client) error {
				_, err := api.MessagesSendMessage(context.Background(), &tg.MessagesSendMessageRequest{Peer: &tg.InputPeerUser{UserID: 42}, Message: "x"})
				return err
			},
			want: []mark{{"42", 11}},
		},
		"群里发媒体": {
			reply: &tg.Updates{Updates: []tg.UpdateClass{
				&tg.UpdateMessageID{ID: 30, RandomID: 5},
				&tg.UpdateNewChannelMessage{Message: channelMessage},
				&tg.UpdateEditChannelMessage{Message: edited},
			}},
			call: func(api *tg.Client) error {
				_, err := api.MessagesSendMedia(context.Background(), &tg.MessagesSendMediaRequest{Peer: &tg.InputPeerChannel{ChannelID: 9}, Media: &tg.InputMediaEmpty{}})
				return err
			},
			want: []mark{{"-1009", 30}},
		},
	} {
		marks = nil
		api := tg.NewClient(invokerFunc(recorder.Handle(replying{c.reply})))
		if err := c.call(api); err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if len(marks) != len(c.want) {
			t.Errorf("%s：记下 %v，应为 %v", name, marks, c.want)
			continue
		}
		for index := range marks {
			if marks[index] != c.want[index] {
				t.Errorf("%s：记下 %v，应为 %v", name, marks, c.want)
			}
		}
	}
}
