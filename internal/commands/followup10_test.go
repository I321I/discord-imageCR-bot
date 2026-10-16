package commands

import (
	"testing"

	"discordbot/internal/index"

	"github.com/bwmarrin/discordgo"
)

// 階段 10 把關後的使用者決定 (B)：附件從索引移除後，「（第 N 張）」維持原訊息中的編號，只剩 1 張也照標。
func TestChoiceName_KeepsOriginalNumberAfterRemoval(t *testing.T) {
	x := index.New()
	img := func(id string) *discordgo.MessageAttachment {
		return &discordgo.MessageAttachment{ID: id, Filename: id + ".png", ContentType: "image/png"}
	}
	x.PutLegacy("g", &discordgo.Message{ID: "m", ChannelID: "c", Content: "舊的", Attachments: []*discordgo.MessageAttachment{img("a0"), img("a1"), img("a2")}})
	x.RemoveAttachment(index.Key{ChannelID: "c", MessageID: "m", Attachment: 0})
	x.RemoveAttachment(index.Key{ChannelID: "c", MessageID: "m", Attachment: 2})
	e, ok := x.Get(index.Key{ChannelID: "c", MessageID: "m", Attachment: 1})
	if !ok {
		t.Fatal("第 2 張應仍在索引中")
	}
	if got := ChoiceName(e); got != "舊的（第 2 張）" {
		t.Errorf("得到 %q，預期「舊的（第 2 張）」", got)
	}
}
