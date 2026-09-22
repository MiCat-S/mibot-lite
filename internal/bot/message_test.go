package bot

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestPeerIDRoundTrip(t *testing.T) {
	cases := map[string]tg.PeerClass{
		"100100100":      &tg.PeerUser{UserID: 100100100},
		"-5115533475":    &tg.PeerChat{ChatID: 5115533475},
		"-1001234567890": &tg.PeerChannel{ChannelID: 1234567890},
	}
	for want, peer := range cases {
		if got := PeerID(peer); got != want {
			t.Errorf("PeerID = %q, want %q", got, want)
		}
		back, ok := PeerFromID(want)
		if !ok {
			t.Errorf("PeerFromID(%q) failed", want)
			continue
		}
		if PeerID(back) != want {
			t.Errorf("round trip of %q produced %q", want, PeerID(back))
		}
	}
	for _, invalid := range []string{"", "abc", "0", "-100"} {
		if _, ok := PeerFromID(invalid); ok {
			t.Errorf("%q should not parse as a peer", invalid)
		}
	}
}

func TestEnvelopeClassifiesChats(t *testing.T) {
	peers := NewPeerCache()
	peers.SetSelf(7)
	peers.RememberChats([]tg.ChatClass{&tg.Channel{ID: 100, AccessHash: 5, Broadcast: true, Title: "news"}})

	private := &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Message: ".ping", Out: true}
	envelope, ok := Envelope(private, 7, false, peers)
	if !ok || envelope.ChatType != ChatPrivate || !envelope.Saved || envelope.SenderID() != 7 {
		t.Fatalf("saved message: %+v ok=%v", envelope, ok)
	}

	broadcast := &tg.Message{ID: 2, PeerID: &tg.PeerChannel{ChannelID: 100}, Post: true}
	envelope, _ = Envelope(broadcast, 7, false, peers)
	if envelope.ChatType != ChatBroadcast {
		t.Fatalf("a known broadcast channel classified as %q", envelope.ChatType)
	}

	group := &tg.Message{ID: 3, PeerID: &tg.PeerChat{ChatID: 55}}
	envelope, _ = Envelope(group, 7, false, peers)
	if envelope.ChatType != ChatGroup || !envelope.IsGroup() {
		t.Fatalf("legacy group classified as %q", envelope.ChatType)
	}
}

func TestEnvelopeReadsReplyAndEdit(t *testing.T) {
	peers := NewPeerCache()
	peers.SetSelf(7)
	message := &tg.Message{ID: 9, PeerID: &tg.PeerChannel{ChannelID: 100}, Message: "hi"}
	message.SetReplyTo(&tg.MessageReplyHeader{ReplyToMsgID: 4, ForumTopic: true})
	message.SetEditDate(1700000000)
	envelope, ok := Envelope(message, 7, false, peers)
	if !ok {
		t.Fatal("envelope failed")
	}
	if envelope.ReplyToID != 4 || envelope.TopicID != 4 {
		t.Fatalf("reply %d topic %d", envelope.ReplyToID, envelope.TopicID)
	}
	if !envelope.Edited {
		t.Fatal("a message with an edit date is edited even when the update was not an edit")
	}
}

// A hash is never guessed: addressing a peer the session has not seen must
// fail rather than send the message somewhere else.
func TestInputPeerRefusesUnknownHash(t *testing.T) {
	peers := NewPeerCache()
	peers.SetSelf(7)
	if _, ok := peers.InputPeer(&tg.PeerChannel{ChannelID: 123}); ok {
		t.Fatal("an unseen channel must not be addressable")
	}
	if _, ok := peers.InputPeer(&tg.PeerChat{ChatID: 55}); !ok {
		t.Fatal("a legacy group needs no access hash")
	}
	peers.RememberChats([]tg.ChatClass{&tg.Channel{ID: 123, AccessHash: 42}})
	resolved, ok := peers.InputPeer(&tg.PeerChannel{ChannelID: 123})
	if !ok || resolved.(*tg.InputPeerChannel).AccessHash != 42 {
		t.Fatalf("resolved %+v ok=%v", resolved, ok)
	}
}

// A min entity's access hash is not usable for addressing, so it must not
// enter the cache: an empty cache is recoverable, a confidently wrong one
// is not.
func TestPeerCacheSkipsMinEntities(t *testing.T) {
	peers := NewPeerCache()
	peers.Remember(tg.Entities{Users: map[int64]*tg.User{5: {ID: 5, AccessHash: 9, Min: true, FirstName: "A"}}})
	if _, ok := peers.InputPeer(&tg.PeerUser{UserID: 5}); ok {
		t.Fatal("a min user must not be addressable")
	}
	if info, known := peers.User(5); !known || info.FirstName != "A" {
		t.Fatal("the name from a min entity is still worth keeping")
	}
}

func TestDisplayName(t *testing.T) {
	if got := (UserInfo{ID: 5, FirstName: "Ada", LastName: "L", Username: "ada"}).DisplayName(); got != "Ada L (@ada)" {
		t.Fatalf("DisplayName = %q", got)
	}
	if got := (UserInfo{ID: 5}).DisplayName(); got != "5" {
		t.Fatalf("a nameless user should show its id, got %q", got)
	}
}

func TestEscapeAndParseHTML(t *testing.T) {
	if got := Escape("<a & b>"); got != "&lt;a &amp; b&gt;" {
		t.Fatalf("Escape = %q", got)
	}
	text, entities, err := ParseHTML("<b>bold</b> plain <code>x</code>")
	if err != nil {
		t.Fatal(err)
	}
	if text != "bold plain x" {
		t.Fatalf("parsed text = %q", text)
	}
	if len(entities) != 2 {
		t.Fatalf("expected two entities, got %d", len(entities))
	}
}

// Telegram's expandable blockquote is what several commands fold long
// output into; gotd only produces it when the attribute survives.
func TestParseHTMLExpandableBlockquote(t *testing.T) {
	_, entities, err := ParseHTML("<blockquote expandable>long</blockquote>")
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 {
		t.Fatalf("expected one entity, got %d", len(entities))
	}
	quote, ok := entities[0].(*tg.MessageEntityBlockquote)
	if !ok || !quote.Collapsed {
		t.Fatalf("expected a collapsed blockquote, got %#v", entities[0])
	}
}

// Replying to part of a message must carry that selection through, or
// .yvlu would quote the whole message the operator deliberately narrowed.
func TestEnvelopeCarriesQuoteSelection(t *testing.T) {
	peers := NewPeerCache()
	peers.SetSelf(7)
	message := &tg.Message{ID: 5, PeerID: &tg.PeerUser{UserID: 7}, Message: ".yvlu"}
	header := &tg.MessageReplyHeader{ReplyToMsgID: 4}
	header.Quote = true
	header.QuoteText = "只要这一段"
	header.QuoteEntities = []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 2}}
	message.SetReplyTo(header)
	envelope, ok := Envelope(message, 7, false, peers)
	if !ok {
		t.Fatal("envelope failed")
	}
	if envelope.QuoteText != "只要这一段" || len(envelope.QuoteEntities) != 1 {
		t.Fatalf("quote selection lost: %q %d entities", envelope.QuoteText, len(envelope.QuoteEntities))
	}
}

// A reply without a selection carries none, so the full message is used.
func TestEnvelopeWithoutQuoteSelection(t *testing.T) {
	peers := NewPeerCache()
	peers.SetSelf(7)
	message := &tg.Message{ID: 5, PeerID: &tg.PeerUser{UserID: 7}}
	message.SetReplyTo(&tg.MessageReplyHeader{ReplyToMsgID: 4})
	envelope, _ := Envelope(message, 7, false, peers)
	if envelope.QuoteText != "" || envelope.QuoteEntities != nil {
		t.Fatalf("unexpected selection: %q", envelope.QuoteText)
	}
}

// The emoji status a user wears is what .yvlu draws beside their name.
func TestPeerCacheKeepsEmojiStatus(t *testing.T) {
	peers := NewPeerCache()
	user := &tg.User{ID: 9, FirstName: "A"}
	user.SetEmojiStatus(&tg.EmojiStatus{DocumentID: 12345})
	peers.RememberUsers([]tg.UserClass{user})
	info, known := peers.User(9)
	if !known || info.EmojiStatus != "12345" {
		t.Fatalf("emoji status is %q", info.EmojiStatus)
	}
}

// A chat with oneself has no direction, so Telegram leaves Out clear.
// Gating commands on that flag discarded every command typed in Saved
// Messages; the question a gate should ask is who wrote the message.
func TestEnvelopeTreatsSavedMessagesAsOwn(t *testing.T) {
	peers := NewPeerCache()
	peers.SetSelf(7)
	message := &tg.Message{ID: 3, PeerID: &tg.PeerUser{UserID: 7}, Message: ".ping"}
	envelope, ok := Envelope(message, 7, false, peers)
	if !ok {
		t.Fatal("envelope failed")
	}
	if !envelope.Saved {
		t.Fatal("a message whose peer is the account is in Saved Messages")
	}
	if envelope.SenderID() != 7 {
		t.Fatalf("sender is %d, want the account", envelope.SenderID())
	}
	if !envelope.Out {
		t.Fatal("the account is the only writer in Saved Messages, so this is its own")
	}
}

// Someone else's private message must not be mistaken for the account's.
func TestEnvelopeKeepsOtherPeopleIncoming(t *testing.T) {
	peers := NewPeerCache()
	peers.SetSelf(7)
	message := &tg.Message{ID: 4, PeerID: &tg.PeerUser{UserID: 99}, Message: ".ping"}
	message.SetFromID(&tg.PeerUser{UserID: 99})
	envelope, ok := Envelope(message, 7, false, peers)
	if !ok {
		t.Fatal("envelope failed")
	}
	if envelope.Out || envelope.Saved {
		t.Fatalf("another user's message: out=%v saved=%v", envelope.Out, envelope.Saved)
	}
	if envelope.SenderID() != 99 {
		t.Fatalf("sender is %d, want 99", envelope.SenderID())
	}
}
