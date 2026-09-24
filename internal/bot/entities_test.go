package bot

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// replying 把 value 编码后写进 output，模拟 Telegram 的应答。
type replying struct{ value bin.Encoder }

func (r replying) Invoke(_ context.Context, _ bin.Encoder, output bin.Decoder) error {
	var buffer bin.Buffer
	if err := r.value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func withHash(user *tg.User) *tg.User { user.SetAccessHash(7); return user }

func TestEntityRecorder(t *testing.T) {
	for name, c := range map[string]struct {
		reply bin.Encoder
		call  func(*tg.Client) error
	}{
		"发消息返回的更新": {
			reply: &tg.Updates{Users: []tg.UserClass{withHash(&tg.User{ID: 42, FirstName: "甲"})},
				Chats: []tg.ChatClass{channel(9)}},
			call: func(api *tg.Client) error {
				_, err := api.MessagesForwardMessages(context.Background(), &tg.MessagesForwardMessagesRequest{FromPeer: &tg.InputPeerSelf{}, ToPeer: &tg.InputPeerSelf{}})
				return err
			},
		},
		"users.getUsers 的向量": {
			reply: &tg.UserClassVector{Elems: []tg.UserClass{withHash(&tg.User{ID: 42, FirstName: "甲"})}},
			call: func(api *tg.Client) error {
				_, err := api.UsersGetUsers(context.Background(), []tg.InputUserClass{&tg.InputUserSelf{}})
				return err
			},
		},
		"解析用户名": {
			reply: &tg.ContactsResolvedPeer{Peer: &tg.PeerUser{UserID: 42}, Users: []tg.UserClass{withHash(&tg.User{ID: 42, FirstName: "甲"})}},
			call: func(api *tg.Client) error {
				_, err := api.ContactsResolveUsername(context.Background(), &tg.ContactsResolveUsernameRequest{Username: "x"})
				return err
			},
		},
	} {
		peers := NewPeerCache()
		recorder := EntityRecorder{Peers: peers}
		api := tg.NewClient(invokerFunc(recorder.Handle(replying{c.reply})))
		if err := c.call(api); err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if _, ok := peers.InputPeer(&tg.PeerUser{UserID: 42}); !ok {
			t.Errorf("%s：用户 42 没记下来", name)
		}
		if name == "发消息返回的更新" {
			if _, ok := peers.InputPeer(&tg.PeerChannel{ChannelID: 9}); !ok {
				t.Errorf("%s：群 9 没记下来", name)
			}
		}
	}
}

type invokerFunc func(ctx context.Context, input bin.Encoder, output bin.Decoder) error

func (f invokerFunc) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	return f(ctx, input, output)
}

func channel(id int64) *tg.Channel {
	value := &tg.Channel{ID: id, Title: "群", Photo: &tg.ChatPhotoEmpty{}}
	value.SetAccessHash(3)
	return value
}

// tg://user 链接：缓存里有这个用户才变成带 access hash 的提及，没有就只留文字，不造假提及。
func TestParseHTMLMentions(t *testing.T) {
	peers := NewPeerCache()
	peers.RememberUsers([]tg.UserClass{withHash(&tg.User{ID: 42, FirstName: "甲"})})
	text := `<a href="tg://user?id=42">甲</a> 和 <a href="tg://user?id=43">乙</a> <b>粗</b>`
	plain, entities, err := parseHTML(text, peers)
	if err != nil || plain != "甲 和 乙 粗" {
		t.Fatalf("%q %v", plain, err)
	}
	mentions, bold := 0, 0
	for _, value := range entities {
		switch typed := value.(type) {
		case *tg.InputMessageEntityMentionName:
			mentions++
			user, ok := typed.UserID.(*tg.InputUser)
			if !ok || user.UserID != 42 || user.AccessHash != 7 {
				t.Errorf("提及的用户不对：%+v", typed.UserID)
			}
		case *tg.MessageEntityBold:
			bold++
		default:
			t.Errorf("多出来的实体 %T", value)
		}
	}
	if mentions != 1 || bold != 1 {
		t.Errorf("提及 %d 个、粗体 %d 个，应各 1 个", mentions, bold)
	}
	if _, entities, _ := ParseHTML(text); len(entities) != 1 {
		t.Errorf("没有缓存时只该有粗体，实际 %d 个实体", len(entities))
	}
}
