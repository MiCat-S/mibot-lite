package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ErrNoMedia means the message carries nothing downloadable.
var ErrNoMedia = errors.New("message has no downloadable media")

// ErrTooLarge means the file exceeds the caller's byte limit.
var ErrTooLarge = errors.New("file exceeds the size limit")

// dcConnectTimeout bounds opening a connection to another data centre.
//
// Opening one is not just a dial: gotd has to run an authorization
// transfer over it before the first request, and that whole sequence can
// stall with no deadline of its own. A download is never so important
// that it may hold a command open indefinitely, so the attempt is capped
// here rather than left to the caller's context.
const dcConnectTimeout = 20 * time.Second

// dcConnection is a held connection to another data centre.
type dcConnection struct {
	api    *tg.Client
	closer io.Closer
}

// mediaDC returns a connection to another data centre, opening one the
// first time and keeping it afterwards.
//
// It asks for an ordinary connection rather than a media-only one. Media
// DCs live on their own address list, and a host that reaches Telegram
// perfectly well may have no route to those: the first attempt at this
// hung inside the pool waiting for a connection that never came up, with
// the session itself healthy on DC 4 the whole time. The ordinary address
// is the one already known to work.
//
// The connection is kept rather than closed after each file. Opening one
// is a full handshake plus an authorization transfer — seconds, not
// milliseconds — and an account whose session is on one data centre and
// whose avatars live on another pays that on every single download. One
// idle socket is a much smaller price than repeating the handshake.
func (c *Client) mediaDC(ctx context.Context, dcID int) (*tg.Client, error) {
	c.dcMu.Lock()
	defer c.dcMu.Unlock()
	if held, ok := c.dcConns[dcID]; ok {
		return held.api, nil
	}
	dial, cancel := context.WithTimeout(ctx, dcConnectTimeout)
	defer cancel()
	invoker, err := c.tg.DC(dial, dcID, 1)
	if err != nil {
		return nil, fmt.Errorf("connect to DC %d: %w", dcID, err)
	}
	api := tg.NewClient(invoker)
	if c.dcConns == nil {
		c.dcConns = map[int]*dcConnection{}
	}
	c.dcConns[dcID] = &dcConnection{api: api, closer: invoker}
	return api, nil
}

// CloseDataCentres releases every extra data-centre connection.
func (c *Client) CloseDataCentres() {
	c.dcMu.Lock()
	defer c.dcMu.Unlock()
	for dcID, held := range c.dcConns {
		_ = held.closer.Close()
		delete(c.dcConns, dcID)
	}
}

// limitedWriter fails once more than limit bytes arrive, so a hostile or
// mistaken file cannot be streamed into memory without bound.
type limitedWriter struct {
	buffer bytes.Buffer
	limit  int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(w.buffer.Len()+len(p)) > w.limit {
		return 0, ErrTooLarge
	}
	return w.buffer.Write(p)
}

// DownloadFile reads a file location into memory, bounded by limit.
//
// The session's own connection is tried first, whatever data centre the
// file claims to live on. Opening a second connection costs a handshake
// and an authorization transfer, and Telegram says plainly when it is
// needed: FILE_MIGRATE_N names the data centre to ask instead. Reaching
// for that connection up front made every avatar pay for it, and one that
// would not come up held the whole command until its deadline.
func (c *Client) DownloadFile(ctx context.Context, location tg.InputFileLocationClass, dcID int, limit int64) ([]byte, error) {
	data, err := download(ctx, c.api, location, limit)
	if err == nil {
		return data, nil
	}
	migrate, isMigrate := tgerr.AsType(err, "FILE_MIGRATE")
	if !isMigrate {
		return nil, err
	}
	target := migrate.Argument
	if target == 0 {
		target = dcID
	}
	if target == 0 {
		return nil, err
	}
	api, dcErr := c.mediaDC(ctx, target)
	if dcErr != nil {
		return nil, dcErr
	}
	return download(ctx, api, location, limit)
}

// download streams one location through the given client.
func download(ctx context.Context, api *tg.Client, location tg.InputFileLocationClass, limit int64) ([]byte, error) {
	sink := &limitedWriter{limit: limit}
	if _, err := downloader.NewDownloader().Download(api, location).Stream(ctx, sink); err != nil {
		return nil, err
	}
	if sink.buffer.Len() == 0 {
		return nil, errors.New("downloaded file is empty")
	}
	return sink.buffer.Bytes(), nil
}

// DownloadProfilePhoto reads a peer's avatar. It reports nil without an
// error when the peer has no photo, which is an ordinary state rather than
// a failure.
func (c *Client) DownloadProfilePhoto(ctx context.Context, peer tg.InputPeerClass, big bool, limit int64) ([]byte, error) {
	photoID, dcID, ok := c.photoOf(peer)
	if !ok {
		return nil, nil
	}
	location := &tg.InputPeerPhotoFileLocation{Peer: peer, PhotoID: photoID, Big: big}
	return c.DownloadFile(ctx, location, dcID, limit)
}

// photoOf finds the cached profile photo id and data centre for a peer.
func (c *Client) photoOf(peer tg.InputPeerClass) (photoID int64, dcID int, ok bool) {
	switch value := peer.(type) {
	case *tg.InputPeerSelf:
		return photoFromUser(c.self)
	case *tg.InputPeerUser:
		if info, known := c.peers.User(value.UserID); known && info.PhotoID != 0 {
			return info.PhotoID, info.PhotoDC, true
		}
	case *tg.InputPeerChannel:
		if info, known := c.peers.Channel(value.ChannelID); known && info.PhotoID != 0 {
			return info.PhotoID, info.PhotoDC, true
		}
	case *tg.InputPeerChat:
		if info, known := c.peers.Chat(value.ChatID); known && info.PhotoID != 0 {
			return info.PhotoID, info.PhotoDC, true
		}
	}
	return 0, 0, false
}

func photoFromUser(user *tg.User) (int64, int, bool) {
	if user == nil {
		return 0, 0, false
	}
	photo, ok := user.GetPhoto()
	if !ok {
		return 0, 0, false
	}
	value, ok := photo.(*tg.UserProfilePhoto)
	if !ok {
		return 0, 0, false
	}
	return value.PhotoID, value.DCID, true
}

// MediaFile describes what a message's media downloaded to.
type MediaFile struct {
	Data     []byte
	MimeType string
	// Sticker reports an attached DocumentAttributeSticker.
	Sticker bool
	// FileName is the document's declared name, when it had one.
	FileName string
}

// DownloadMedia reads a message's photo or document, bounded by limit.
func (c *Client) DownloadMedia(ctx context.Context, message *tg.Message, limit int64) (*MediaFile, error) {
	media, ok := message.GetMedia()
	if !ok {
		return nil, ErrNoMedia
	}
	switch value := media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := value.Photo.(*tg.Photo)
		if !ok {
			return nil, ErrNoMedia
		}
		size := largestPhotoSize(photo.Sizes)
		if size == "" {
			return nil, ErrNoMedia
		}
		location := &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: size}
		data, err := c.DownloadFile(ctx, location, photo.DCID, limit)
		if err != nil {
			return nil, err
		}
		return &MediaFile{Data: data, MimeType: "image/jpeg"}, nil
	case *tg.MessageMediaDocument:
		document, ok := value.Document.(*tg.Document)
		if !ok {
			return nil, ErrNoMedia
		}
		if int64(document.Size) > limit {
			return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, document.Size)
		}
		location := &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference}
		data, err := c.DownloadFile(ctx, location, document.DCID, limit)
		if err != nil {
			return nil, err
		}
		file := &MediaFile{Data: data, MimeType: document.MimeType}
		for _, attribute := range document.Attributes {
			switch typed := attribute.(type) {
			case *tg.DocumentAttributeSticker:
				file.Sticker = true
			case *tg.DocumentAttributeFilename:
				file.FileName = typed.FileName
			}
		}
		return file, nil
	}
	return nil, ErrNoMedia
}

// largestPhotoSize picks the biggest ordinary size a photo offers.
func largestPhotoSize(sizes []tg.PhotoSizeClass) string {
	best, bestArea := "", 0
	for _, entry := range sizes {
		switch size := entry.(type) {
		case *tg.PhotoSize:
			if area := size.W * size.H; area > bestArea {
				best, bestArea = size.Type, area
			}
		case *tg.PhotoSizeProgressive:
			if area := size.W * size.H; area > bestArea {
				best, bestArea = size.Type, area
			}
		}
	}
	return best
}

// DocumentOf returns the InputDocument for a message's document, which is
// what the sticker RPCs take.
func DocumentOf(message *tg.Message) (*tg.InputDocument, bool) {
	media, ok := message.GetMedia()
	if !ok {
		return nil, false
	}
	value, ok := media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	document, ok := value.Document.(*tg.Document)
	if !ok {
		return nil, false
	}
	return &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference}, true
}
