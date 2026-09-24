package ai

import (
	"context"
	"errors"
	_ "image/gif"
	"regexp"
	"sort"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/imaging"
)

// 提问时带上的图片：回复的那条和命令消息本身里的照片、图片文件、贴纸和动图
// （取缩略图或首帧），相册按整组收。数量和体积的上限与 MiBox 一致：
// 合计最多 4 张、20 MiB，超出的丢弃并提示。
const (
	aiMaxImages     = 4
	aiMaxImageBytes = 20 << 20
	// aiAlbumSpan 是找相册其余成员时，在这条消息前后各查的编号数；
	// 一个相册最多 10 条，发送时编号连续。
	aiAlbumSpan = 9
)

var aiImageMime = regexp.MustCompile(`(?i)^image/(?:jpeg|png|gif|webp)$`)

// imageSet 是收集到的图片，dropped 表示有图片因为数量或体积被丢弃。
type imageSet struct {
	images  []aiImage
	dropped bool
}

// mergeImages 按顺序合并几组图片，合计不超过 4 张、20 MiB。
func mergeImages(sets ...imageSet) imageSet {
	var merged imageSet
	total := 0
	for _, set := range sets {
		merged.dropped = merged.dropped || set.dropped
		for _, image := range set.images {
			if len(merged.images) >= aiMaxImages || total+len(image.Data) > aiMaxImageBytes {
				merged.dropped = true
				continue
			}
			total += len(image.Data)
			merged.images = append(merged.images, image)
		}
	}
	return merged
}

// collectImages 收集一条消息里的图片；消息属于相册时收整组（按编号排序，最多 4 条）。
func collectImages(ctx context.Context, client *bot.Client, message *bot.Message) (imageSet, error) {
	if message == nil || message.Raw == nil {
		return imageSet{}, nil
	}
	raw := message.Raw
	if _, ok := raw.GetMedia(); !ok {
		return imageSet{}, nil
	}
	members := []*tg.Message{raw}
	var set imageSet
	if grouped, ok := raw.GetGroupedID(); ok && grouped != 0 {
		members = albumMembers(ctx, client, message, grouped)
		if len(members) > aiMaxImages {
			members, set.dropped = members[:aiMaxImages], true
		}
	}
	var collected []aiImage
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return imageSet{}, err
		}
		image, err := imageOf(ctx, client, member)
		if err != nil {
			return imageSet{}, kit.Fail("图片下载失败：" + command.Brief(err))
		}
		if image != nil {
			collected = append(collected, *image)
		}
	}
	merged := mergeImages(imageSet{images: collected})
	merged.dropped = merged.dropped || set.dropped
	return merged, nil
}

// albumMembers 读取和 message 同一相册的消息；读不到时只用它自己。
func albumMembers(ctx context.Context, client *bot.Client, message *bot.Message, grouped int64) []*tg.Message {
	peer, err := client.InputPeer(message.Peer)
	if err != nil {
		return []*tg.Message{message.Raw}
	}
	var ids []int
	for id := message.ID - aiAlbumSpan; id <= message.ID+aiAlbumSpan; id++ {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	found, err := client.GetMessages(ctx, peer, ids)
	if err != nil {
		return []*tg.Message{message.Raw}
	}
	var members []*tg.Message
	for _, candidate := range found {
		if id, ok := candidate.GetGroupedID(); ok && id == grouped {
			members = append(members, candidate)
		}
	}
	if len(members) == 0 {
		return []*tg.Message{message.Raw}
	}
	sort.Slice(members, func(a, b int) bool { return members[a].ID < members[b].ID })
	return members
}

// animatedDocument 判断文档是不是动图、视频贴纸或动画贴纸：这些只能取静态的缩略图或首帧。
func animatedDocument(document *tg.Document) bool {
	switch strings.ToLower(document.MimeType) {
	case "image/gif", "video/webm", "application/x-tgsticker", "application/x-tg-sticker":
		return true
	}
	for _, attribute := range document.Attributes {
		if _, ok := attribute.(*tg.DocumentAttributeAnimated); ok {
			return true
		}
	}
	return false
}

// imageOf 取一条消息里可以发给模型的图片：照片和静态图片文件原样下载；
// 其他文档（动图、贴纸、视频等）取最大的缩略图转成 PNG，GIF 没有缩略图时取首帧。
// 没有可用图片或文件超过上限时返回 nil。
func imageOf(ctx context.Context, client *bot.Client, message *tg.Message) (*aiImage, error) {
	media, ok := message.GetMedia()
	if !ok {
		return nil, nil
	}
	switch value := media.(type) {
	case *tg.MessageMediaPhoto:
		file, err := client.DownloadMedia(ctx, message, aiMaxImageBytes)
		if err != nil {
			return skipOversize(err)
		}
		return &aiImage{Data: file.Data, MimeType: "image/jpeg"}, nil
	case *tg.MessageMediaDocument:
		document, ok := value.Document.(*tg.Document)
		if !ok {
			return nil, nil
		}
		mime := strings.ToLower(document.MimeType)
		if !animatedDocument(document) && aiImageMime.MatchString(mime) {
			if document.Size > aiMaxImageBytes {
				return nil, nil
			}
			file, err := client.DownloadMedia(ctx, message, aiMaxImageBytes)
			if err != nil {
				return skipOversize(err)
			}
			return &aiImage{Data: file.Data, MimeType: mime}, nil
		}
		if data, err := documentThumb(ctx, client, document); err != nil {
			return skipOversize(err)
		} else if data != nil {
			if png, err := firstFramePNG(data); err == nil {
				return &aiImage{Data: png, MimeType: "image/png"}, nil
			}
		}
		if mime == "image/gif" && document.Size <= aiMaxImageBytes {
			file, err := client.DownloadMedia(ctx, message, aiMaxImageBytes)
			if err != nil {
				return skipOversize(err)
			}
			if png, err := firstFramePNG(file.Data); err == nil {
				return &aiImage{Data: png, MimeType: "image/png"}, nil
			}
		}
	}
	return nil, nil
}

func skipOversize(err error) (*aiImage, error) {
	if errors.Is(err, bot.ErrTooLarge) || errors.Is(err, bot.ErrNoMedia) {
		return nil, nil
	}
	return nil, err
}

// thumbChoice 从缩略图列表里挑面积最大的一张：可下载的给出尺寸类型，
// 内嵌在消息里的直接给出字节。
func thumbChoice(thumbs []tg.PhotoSizeClass) (kind string, inline []byte) {
	area := 0
	for _, entry := range thumbs {
		switch size := entry.(type) {
		case *tg.PhotoSize:
			if size.W*size.H > area {
				kind, inline, area = size.Type, nil, size.W*size.H
			}
		case *tg.PhotoSizeProgressive:
			if size.W*size.H > area {
				kind, inline, area = size.Type, nil, size.W*size.H
			}
		case *tg.PhotoCachedSize:
			if size.W*size.H > area {
				kind, inline, area = "", size.Bytes, size.W*size.H
			}
		}
	}
	return kind, inline
}

// documentThumb 下载文档最大的缩略图，没有缩略图时返回 nil。
func documentThumb(ctx context.Context, client *bot.Client, document *tg.Document) ([]byte, error) {
	kind, inline := thumbChoice(document.Thumbs)
	if inline != nil {
		return inline, nil
	}
	if kind == "" {
		return nil, nil
	}
	location := &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference, ThumbSize: kind}
	return client.DownloadFile(ctx, location, document.DCID, aiMaxImageBytes)
}

// firstFramePNG 解码 JPEG、PNG、WebP 或 GIF（取第一帧），重新编码成 PNG。
func firstFramePNG(data []byte) ([]byte, error) {
	decoded, err := imaging.Decode(data)
	if err != nil {
		return nil, err
	}
	return imaging.EncodePNG(decoded)
}
