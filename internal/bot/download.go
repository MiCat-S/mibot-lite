package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ErrNoMedia 表示消息里没有可下载的内容。
var ErrNoMedia = errors.New("message has no downloadable media")

// ErrTooLarge 表示文件超过了调用方给的字节上限。
var ErrTooLarge = errors.New("file exceeds the size limit")

// dcConnectTimeout 限制连接另一个数据中心最多花多长时间。
//
// 建立连接不只是拨号：发第一个请求之前，gotd 要先在这条连接上做一次
// 授权转移，整个过程可能卡住，而它自己并没有超时。下载再重要，也不值得
// 让一条命令无限期挂着，所以在这里给这次尝试封顶，不交给调用方的 context。
const dcConnectTimeout = 20 * time.Second

// dcConnection 是一条保留着的、通往另一个数据中心的连接。
type dcConnection struct {
	api    *tg.Client
	closer io.Closer
}

// mediaDC 返回通往另一个数据中心的连接：第一次用时打开，之后一直留着。
//
// 这里要的是普通连接，不是 media-only 连接。媒体 DC 有自己单独的地址
// 列表，一台能正常连上 Telegram 的主机，未必连得到那些地址：最初的实现
// 就卡在连接池里，等一条永远建不起来的连接，而会话本身在 DC 4 上一直
// 好好的。普通地址是已经确认能通的那个。
//
// 连接会一直保留，不在每个文件下载完后关掉。建立一条连接要做完整的握手
// 再加一次授权转移，耗时以秒计而不是毫秒；如果账号的会话在一个数据中心、
// 头像在另一个，每次下载都要付出这个代价。多留一个空闲的 socket，比反复
// 握手便宜得多。
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

// CloseDataCentres 释放所有额外的数据中心连接。
func (c *Client) CloseDataCentres() {
	c.dcMu.Lock()
	defer c.dcMu.Unlock()
	for dcID, held := range c.dcConns {
		_ = held.closer.Close()
		delete(c.dcConns, dcID)
	}
}

// limitedWriter 在收到的字节超过 limit 时报错，免得恶意或出错的文件
// 被无限制地读进内存。
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

// DownloadFile 把一个文件位置的内容读进内存，大小受 limit 限制。
//
// 不管文件声称在哪个数据中心，都先用会话自己的连接试。再开一条连接要
// 付出一次握手加一次授权转移，而真正需要时 Telegram 会明说：
// FILE_MIGRATE_N 会指明该改去问哪个数据中心。以前一上来就去开那条连接，
// 每个头像都要为此付出代价；连接建不起来时，整条命令会一直卡到超时。
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

// download 通过给定的客户端，流式下载一个文件位置。
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

// DownloadProfilePhoto 读取 peer 的头像。peer 没有头像时返回 nil，
// 不报错，因为这是正常状态，不算失败。
func (c *Client) DownloadProfilePhoto(ctx context.Context, peer tg.InputPeerClass, big bool, limit int64) ([]byte, error) {
	photoID, dcID, ok := c.photoOf(peer)
	if !ok {
		return nil, nil
	}
	location := &tg.InputPeerPhotoFileLocation{Peer: peer, PhotoID: photoID, Big: big}
	return c.DownloadFile(ctx, location, dcID, limit)
}

// photoOf 从缓存里找出 peer 的头像 id 和所在的数据中心。
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

// MediaFile 描述消息媒体下载下来的结果。
type MediaFile struct {
	Data     []byte
	MimeType string
	// Sticker 表示文档带有 DocumentAttributeSticker。
	Sticker bool
	// FileName 是文档声明的文件名，如果有的话。
	FileName string
}

// DownloadMedia 读取消息里的照片或文档，大小受 limit 限制。
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

// largestPhotoSize 从照片提供的普通尺寸里挑出最大的一个。
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

// DocumentOf 返回消息里文档对应的 InputDocument，贴纸相关的 RPC
// 要的就是它。
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

// MediaSource 记录消息里的照片或文档在哪里，以及重新上传、再发一份
// 同样的内容所需的信息。
type MediaSource struct {
	Location tg.InputFileLocationClass
	DCID     int
	Size     int64
	Photo    bool
	MimeType string
	FileName string
	// Attributes 是文档自身的属性：视频的时长和尺寸、语音标志、贴纸集、
	// 动图标记。重新上传时沿用它们，圆形视频才仍是圆形视频，语音消息才仍是
	// 语音消息，而不是一个叫 audio.ogg 的文件。
	Attributes []tg.DocumentAttributeClass
}

// SourceOf 找出消息里可下载的照片或文档。
func SourceOf(message *tg.Message) (*MediaSource, bool) {
	media, ok := message.GetMedia()
	if !ok {
		return nil, false
	}
	switch value := media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := value.Photo.(*tg.Photo)
		if !ok {
			return nil, false
		}
		size := largestPhotoSize(photo.Sizes)
		if size == "" {
			return nil, false
		}
		return &MediaSource{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: size},
			DCID:     photo.DCID, Photo: true, MimeType: "image/jpeg",
		}, true
	case *tg.MessageMediaDocument:
		document, ok := value.Document.(*tg.Document)
		if !ok {
			return nil, false
		}
		source := &MediaSource{
			Location: &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
			DCID:     document.DCID, Size: document.Size, MimeType: document.MimeType, Attributes: document.Attributes,
		}
		for _, attribute := range document.Attributes {
			if name, ok := attribute.(*tg.DocumentAttributeFilename); ok {
				source.FileName = name.FileName
			}
		}
		return source, true
	}
	return nil, false
}

// DownloadTo 把文件直接流式写到磁盘。
//
// DownloadFile 会把整个文件放在内存里，对头像合适，对 2 GB 的视频就不行。
// 这里边下边写，每次四个分片，遇到 FILE_MIGRATE 也照样转过去。
func (c *Client) DownloadTo(ctx context.Context, source *MediaSource, file *os.File) error {
	err := parallel(ctx, c.api, source.Location, file)
	if err == nil {
		return nil
	}
	migrate, isMigrate := tgerr.AsType(err, "FILE_MIGRATE")
	if !isMigrate {
		return err
	}
	target := migrate.Argument
	if target == 0 {
		target = source.DCID
	}
	if target == 0 {
		return err
	}
	api, dcErr := c.mediaDC(ctx, target)
	if dcErr != nil {
		return dcErr
	}
	// migrate 错误会在任何分片到达之前出现，但文件还是先清空：
	// 重试绝不能写在半截文件上面。
	if err := file.Truncate(0); err != nil {
		return err
	}
	return parallel(ctx, api, source.Location, file)
}

func parallel(ctx context.Context, api *tg.Client, location tg.InputFileLocationClass, file *os.File) error {
	_, err := downloader.NewDownloader().Download(api, location).WithThreads(4).Parallel(ctx, file)
	return err
}
