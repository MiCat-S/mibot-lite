package bot

import (
	"context"
	"sync"
	"testing"

	"github.com/gotd/td/tg"
)

// fakeStore 是内存里的持久化 hash 存储，顺便记下写入次数。
type fakeStore struct {
	mu            sync.Mutex
	users         map[int64]int64
	channels      map[int64]int64
	userWrites    int
	channelWrites int
}

func newFakeStore() *fakeStore {
	return &fakeStore{users: map[int64]int64{}, channels: map[int64]int64{}}
}

func (s *fakeStore) GetUserAccessHash(_ context.Context, _, targetUserID int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.users[targetUserID]
	return hash, ok, nil
}

func (s *fakeStore) GetChannelAccessHash(_ context.Context, _, channelID int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.channels[channelID]
	return hash, ok, nil
}

func (s *fakeStore) SetUserAccessHash(_ context.Context, _, targetUserID, accessHash int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[targetUserID] = accessHash
	s.userWrites++
	return nil
}

func (s *fakeStore) SetChannelAccessHash(_ context.Context, _, channelID, accessHash int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[channelID] = accessHash
	s.channelWrites++
	return nil
}

func (s *fakeStore) user(id int64) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.users[id]
	return hash, ok
}

func (s *fakeStore) channel(id int64) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.channels[id]
	return hash, ok
}

func (s *fakeStore) writes() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userWrites, s.channelWrites
}

// RPC 应答里学到的用户 hash 写穿到持久化存储；min 和 hash 为 0 的不写，同一个 hash
// 第二次不重复写。
func TestRememberUsersWritesThrough(t *testing.T) {
	store := newFakeStore()
	peers := NewPeerCache()
	peers.SetSelf(1)
	peers.SetDurable(store)

	peers.RememberUsers([]tg.UserClass{
		&tg.User{ID: 42, AccessHash: 7},
		&tg.User{ID: 43},
		&tg.User{ID: 44, AccessHash: 9, Min: true},
	})
	if hash, ok := store.user(42); !ok || hash != 7 {
		t.Errorf("用户 42 的 hash 没写进存储：%d %v", hash, ok)
	}
	if _, ok := store.user(43); ok {
		t.Error("hash 为 0 的用户不该写")
	}
	if _, ok := store.user(44); ok {
		t.Error("min 用户不该写")
	}
	if users, _ := store.writes(); users != 1 {
		t.Errorf("应只写一次，实际 %d 次", users)
	}

	// 同一个 hash 再来一次：内存已记住，不重复落盘。
	peers.RememberUsers([]tg.UserClass{&tg.User{ID: 42, AccessHash: 7}})
	if users, _ := store.writes(); users != 1 {
		t.Errorf("同一个 hash 不该重复写，实际 %d 次", users)
	}

	// hash 变了：写新值。
	peers.RememberUsers([]tg.UserClass{&tg.User{ID: 42, AccessHash: 8}})
	if hash, ok := store.user(42); !ok || hash != 8 {
		t.Errorf("换 hash 后应写新值：%d %v", hash, ok)
	}
	if users, _ := store.writes(); users != 2 {
		t.Errorf("换 hash 应再写一次，实际 %d 次", users)
	}
}

// 频道和 ChannelForbidden 带 hash 时同样写穿；min 频道不写。
func TestRememberChatsWritesThrough(t *testing.T) {
	store := newFakeStore()
	peers := NewPeerCache()
	peers.SetSelf(1)
	peers.SetDurable(store)

	peers.RememberChats([]tg.ChatClass{
		&tg.Channel{ID: 55, AccessHash: 11},
		&tg.Channel{ID: 56, AccessHash: 12, Min: true},
		&tg.ChannelForbidden{ID: 57, AccessHash: 13},
	})
	if hash, ok := store.channel(55); !ok || hash != 11 {
		t.Errorf("频道 55 的 hash 没写进存储：%d %v", hash, ok)
	}
	if _, ok := store.channel(56); ok {
		t.Error("min 频道不该写")
	}
	if hash, ok := store.channel(57); !ok || hash != 13 {
		t.Errorf("ChannelForbidden 的 hash 没写进存储：%d %v", hash, ok)
	}
	if _, channels := store.writes(); channels != 2 {
		t.Errorf("应写两次，实际 %d 次", channels)
	}

	peers.RememberChats([]tg.ChatClass{&tg.Channel{ID: 55, AccessHash: 11}})
	if _, channels := store.writes(); channels != 2 {
		t.Errorf("同一个 hash 不该重复写，实际 %d 次", channels)
	}
}

// 「重启」：新进程内存里什么都没有，靠持久化存储里的 hash 仍能寻址。
func TestInputPeerAfterRestart(t *testing.T) {
	store := newFakeStore()
	first := NewPeerCache()
	first.SetSelf(1)
	first.SetDurable(store)
	first.RememberUsers([]tg.UserClass{&tg.User{ID: 42, AccessHash: 7}})
	first.RememberChats([]tg.ChatClass{&tg.Channel{ID: 55, AccessHash: 11}})

	restarted := NewPeerCache()
	restarted.SetSelf(1)
	restarted.SetDurable(store)
	peer, ok := restarted.InputPeer(&tg.PeerUser{UserID: 42})
	if !ok {
		t.Fatal("重启后应能从持久化存储拿到用户")
	}
	user, ok := peer.(*tg.InputPeerUser)
	if !ok || user.UserID != 42 || user.AccessHash != 7 {
		t.Errorf("用户 input peer 不对：%+v", peer)
	}
	peer, ok = restarted.InputPeer(&tg.PeerChannel{ChannelID: 55})
	if !ok {
		t.Fatal("重启后应能从持久化存储拿到频道")
	}
	channel, ok := peer.(*tg.InputPeerChannel)
	if !ok || channel.ChannelID != 55 || channel.AccessHash != 11 {
		t.Errorf("频道 input peer 不对：%+v", peer)
	}
}

// 只实现了 HashLookup（没有 Set 方法）的 durable 不会被写，也不 panic。
func TestDurableWithoutWriterIsSafe(t *testing.T) {
	var lookup HashLookup = lookupOnly{}
	peers := NewPeerCache()
	peers.SetSelf(1)
	peers.SetDurable(lookup)
	peers.RememberUsers([]tg.UserClass{&tg.User{ID: 42, AccessHash: 7}})
	peers.RememberChats([]tg.ChatClass{&tg.Channel{ID: 55, AccessHash: 11}})
	if _, ok := peers.InputPeer(&tg.PeerUser{UserID: 42}); !ok {
		t.Error("内存缓存仍应记下用户")
	}
}

// lookupOnly 故意只实现 HashLookup，不实现 HashStore。
type lookupOnly struct{}

func (lookupOnly) GetUserAccessHash(context.Context, int64, int64) (int64, bool, error) {
	return 0, false, nil
}

func (lookupOnly) GetChannelAccessHash(context.Context, int64, int64) (int64, bool, error) {
	return 0, false, nil
}

// 一次应答带来超过 maxPersistBatch 条新 hash（成员列表、对话列表）时只记在内存，不写盘；
// 内存里照样能寻址。
func TestBulkHashesStayInMemory(t *testing.T) {
	store := newFakeStore()
	peers := NewPeerCache()
	peers.SetSelf(1)
	peers.SetDurable(store)
	var bulk []tg.UserClass
	for id := int64(100); id < 100+maxPersistBatch+1; id++ {
		bulk = append(bulk, &tg.User{ID: id, AccessHash: id * 3})
	}
	peers.RememberUsers(bulk)
	if users, _ := store.writes(); users != 0 {
		t.Errorf("大批的 hash 不该写盘，实际写了 %d 次", users)
	}
	if peer, ok := peers.InputPeer(&tg.PeerUser{UserID: 100}); !ok || peer.(*tg.InputPeerUser).AccessHash != 300 {
		t.Errorf("内存里应该还能寻址：%v %v", peer, ok)
	}
}
