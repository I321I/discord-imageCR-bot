package recognize

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// 把關第二輪：HasImagePreview 的網址比對不可被相近的網址騙過（預覽屬於 A，貼出的卻是 B）。
func TestGateR2_HasImagePreview_SpoofVariants(t *testing.T) {
	const real = "https://i.imgur.com/abc.jpg"
	img := func(u string) []*discordgo.MessageEmbed {
		return []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: u}}
	}
	thumb := func(u string) []*discordgo.MessageEmbed {
		return []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeArticle, URL: "https://x.com", Thumbnail: &discordgo.MessageEmbedThumbnail{URL: u}}}
	}
	notSame := []string{
		"https://i.imgur.com.evil.com/abc.jpg", // 後綴網域
		"https://i.imgur.com@evil.com/abc.jpg", // userinfo
		"https://evil.com/abc.jpg?https://i.imgur.com/abc.jpg",
		"https://evil.com/https://i.imgur.com/abc.jpg",
		"https://i.imgur.com/abc.jpg/../../evil", // 路徑不同
		"http://i.imgur.com:8080/abc.jpg",
		"https://i.imgur.com/ABC.jpg", // 路徑大小寫不同
		"https://i.imgur.com/abc.jpg#x",
		"https://i.imgur.com/abc.jpg?x=1",
	}
	for _, raw := range notSame {
		if HasImagePreview(raw, img(real)) {
			t.Errorf("image 預覽屬於 %s，不應當成 %s 的預覽", real, raw)
		}
		if HasImagePreview(raw, thumb(real)) {
			t.Errorf("thumbnail 屬於 %s，不應當成 %s 的預覽", real, raw)
		}
	}
	// 只差 host 大小寫、scheme 大小寫：同一網址。
	for _, raw := range []string{"https://I.IMGUR.COM/abc.jpg", "HTTPS://i.imgur.com/abc.jpg"} {
		if !HasImagePreview(raw, img(real)) {
			t.Errorf("%s 應與 %s 視為同一網址", raw, real)
		}
	}
	// nil embed、空網址的預覽不算。
	if HasImagePreview(real, []*discordgo.MessageEmbed{nil, {Type: discordgo.EmbedTypeImage}, {Image: &discordgo.MessageEmbedImage{}}, {Thumbnail: &discordgo.MessageEmbedThumbnail{}}}) {
		t.Error("空的預覽不應算")
	}
	if HasImagePreview("", img("")) {
		t.Error("空網址不應算")
	}
}
