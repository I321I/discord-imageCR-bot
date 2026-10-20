package recognize

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	example1 = "mygo、我早就知道會這樣了\nhttps://i.imgur.com/jgoYzsS.jpg"
	example2 = "https://i.imgur.com/avJMxmH.jpg\n我不一想說得太失禮 你還是放棄吧"
)

func linkURLs(ls []Link) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.URL)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLinks_Examples(t *testing.T) {
	cases := []struct {
		content, url string
	}{
		{example1, "https://i.imgur.com/jgoYzsS.jpg"},
		{example2, "https://i.imgur.com/avJMxmH.jpg"},
	}
	for _, c := range cases {
		ls := Links(c.content)
		if len(ls) != 1 || ls[0].URL != c.url {
			t.Fatalf("Links(%q) = %+v", c.content, ls)
		}
		if got := c.content[ls[0].Start:ls[0].End]; got != c.url {
			t.Errorf("位置不正確：%q", got)
		}
		if ls[0].Keep != "" || ls[0].Spoiler {
			t.Errorf("一般網址不應有 Keep／Spoiler：%+v", ls[0])
		}
	}
}

func TestLinks_TrailingPunctuation(t *testing.T) {
	cases := map[string]string{
		"看這個 https://a.com/x.png。":         "https://a.com/x.png",
		"(https://a.com/x.png)":            "https://a.com/x.png",
		"https://a.com/x.png, 和其他":         "https://a.com/x.png",
		"https://a.com/x.png!":             "https://a.com/x.png",
		"「https://a.com/x.png」":            "https://a.com/x.png",
		"https://en.wiki.org/a_(b)":        "https://en.wiki.org/a_(b)",
		"https://a.com/x.png?w=1&h=2.":     "https://a.com/x.png?w=1&h=2",
		"前文https://a.com/x.png 後文":         "https://a.com/x.png",
		"http://a.com/x.png\thttps://b.c/": "http://a.com/x.png",
	}
	for content, want := range cases {
		ls := Links(content)
		if len(ls) == 0 || ls[0].URL != want {
			t.Errorf("Links(%q) = %v，預期第一個為 %q", content, linkURLs(ls), want)
		}
	}
	if ls := Links("ftp://a.com/x.png httpx://b.com"); len(ls) != 0 {
		t.Errorf("非 http/https 不算連結：%v", linkURLs(ls))
	}
}

// U6：同一網址只算一次（保留第一個）。
func TestLinks_Dedup(t *testing.T) {
	ls := Links("https://a.com/x.png 再一次 https://a.com/x.png https://a.com/y.png")
	if got := linkURLs(ls); !equalStrings(got, []string{"https://a.com/x.png", "https://a.com/y.png"}) {
		t.Errorf("Links = %v", got)
	}
	if ls[0].Start != 0 {
		t.Errorf("應保留第一個出現的位置：%d", ls[0].Start)
	}
}

// U7：<網址>、||網址||、[文字](網址)。
func TestLinks_Wrappers(t *testing.T) {
	cases := []struct {
		content, url, removed, keep string
		spoiler                     bool
	}{
		{"看 <https://a.com/x.png> 喔", "https://a.com/x.png", "<https://a.com/x.png>", "", false},
		{"看 ||https://a.com/x.png|| 喔", "https://a.com/x.png", "||https://a.com/x.png||", "", true},
		{"看 [圖](https://a.com/x.png) 喔", "https://a.com/x.png", "[圖](https://a.com/x.png)", "圖", false},
		{"||劇透 https://a.com/x.png 劇透||", "https://a.com/x.png", "https://a.com/x.png", "", true},
		{"||[圖](https://a.com/x.png)||", "https://a.com/x.png", "[圖](https://a.com/x.png)", "圖", true},
	}
	for _, c := range cases {
		ls := Links(c.content)
		if len(ls) != 1 {
			t.Fatalf("Links(%q) = %+v", c.content, ls)
		}
		l := ls[0]
		if l.URL != c.url || c.content[l.Start:l.End] != c.removed || l.Keep != c.keep || l.Spoiler != c.spoiler {
			t.Errorf("Links(%q) = %+v（移除 %q），預期 URL %q 移除 %q Keep %q Spoiler %v",
				c.content, l, c.content[l.Start:l.End], c.url, c.removed, c.keep, c.spoiler)
		}
	}
}

func TestIsImageLink_Extension(t *testing.T) {
	yes := []string{
		"https://a.com/x.JPG", "https://a.com/x.jpeg?width=100", "https://a.com/x.webp#x",
		"http://a.com/x.png", "https://a.com/dir/x.gif",
	}
	no := []string{
		"https://a.com/x.jpg.html", "https://a.com/x.png/page", "ftp://a/x.png", "javascript:x.png",
		"https:///x.png", "https://a.com/x", "https://a.com/x.mp4",
	}
	for _, u := range yes {
		m := &discordgo.Message{Content: u}
		if srcs, ok := ImageSources(m); !ok || len(srcs) != 1 || srcs[0].URL != u || srcs[0].FetchURL != u {
			t.Errorf("%q 應為圖片網址：%+v %v", u, srcs, ok)
		}
	}
	for _, u := range no {
		m := &discordgo.Message{Content: u}
		if srcs, _ := ImageSources(m); len(srcs) != 0 {
			t.Errorf("%q 不應為圖片網址：%+v", u, srcs)
		}
	}
}

func TestIsImageLink_Embed(t *testing.T) {
	const u = "https://example.com/abc"
	cases := []struct {
		name   string
		embeds []*discordgo.MessageEmbed
		want   bool
		fetch  string
		gifv   bool
	}{
		{"image 型且 URL 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: u}}, true, u, false},
		{"image 型、縮圖相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: u, Thumbnail: &discordgo.MessageEmbedThumbnail{URL: u}}}, true, u, false},
		{"image 型、縮圖為別的網址（U4：下載縮圖）", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: u, Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://i.example.com/abc.jpg"}}}, true, "https://i.example.com/abc.jpg", false},
		{"image 型、image 為別的網址", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: u, Image: &discordgo.MessageEmbedImage{URL: "https://i.example.com/big.png"}}}, true, "https://i.example.com/big.png", false},
		{"article 但縮圖相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeArticle, URL: "https://other.com", Thumbnail: &discordgo.MessageEmbedThumbnail{URL: u}}}, true, u, false},
		{"article 且縮圖不同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeArticle, URL: u, Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://example.com/t.jpg"}}}, false, "", false},
		{"embed 屬於別的連結", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: "https://other.com/x"}}, false, "", false},
		{"gifv", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeGifv, URL: u, Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://media.example.com/t.png"}}}, true, "https://media.example.com/t.png", true},
		{"URL 編碼差異視為相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: "https://EXAMPLE.com/abc"}}, true, u, false},
	}
	for _, c := range cases {
		m := &discordgo.Message{Content: "看 " + u, Embeds: c.embeds}
		srcs, ok := ImageSources(m)
		if !ok {
			t.Fatalf("%s：ok 應為 true", c.name)
		}
		if got := len(srcs) == 1; got != c.want {
			t.Errorf("%s：是否為圖片 = %v，預期 %v（%+v）", c.name, got, c.want, srcs)
			continue
		}
		if c.want && (srcs[0].URL != u || srcs[0].FetchURL != c.fetch || srcs[0].GIFV != c.gifv) {
			t.Errorf("%s：%+v，預期 FetchURL %q GIFV %v", c.name, srcs[0], c.fetch, c.gifv)
		}
	}
}

// U8：附件在前（依附件順序）、網址在後（依文字順序）；Index 為附件位置／Links 中的位置。
func TestImageSources_MixedOrder(t *testing.T) {
	m := &discordgo.Message{
		Content: "a https://web.com/page b https://a.com/1.png c ||https://a.com/2.jpg||",
		Attachments: []*discordgo.MessageAttachment{
			{ID: "a0", Filename: "x.png", ContentType: "image/png"},
			{ID: "a1", Filename: "SPOILER_y.jpg", ContentType: "image/jpeg"},
		},
	}
	srcs, ok := ImageSources(m)
	if !ok || len(srcs) != 4 {
		t.Fatalf("應有 4 個來源：%+v %v", srcs, ok)
	}
	if srcs[0].Attachment != m.Attachments[0] || srcs[0].Index != 0 || srcs[0].IsLink() || srcs[0].Spoiler {
		t.Errorf("第 1 個應為附件 0：%+v", srcs[0])
	}
	if srcs[1].Attachment != m.Attachments[1] || srcs[1].Index != 1 || !srcs[1].Spoiler {
		t.Errorf("第 2 個應為附件 1（劇透）：%+v", srcs[1])
	}
	if !srcs[2].IsLink() || srcs[2].URL != "https://a.com/1.png" || srcs[2].Index != 1 || srcs[2].Spoiler {
		t.Errorf("第 3 個應為網址 1.png（Links 位置 1）：%+v", srcs[2])
	}
	if !srcs[3].IsLink() || srcs[3].URL != "https://a.com/2.jpg" || srcs[3].Index != 2 || !srcs[3].Spoiler {
		t.Errorf("第 4 個應為網址 2.jpg（Links 位置 2、劇透）：%+v", srcs[3])
	}
}

func TestImageSources_NonImageAttachment(t *testing.T) {
	m := &discordgo.Message{
		Content:     "https://a.com/x.png",
		Attachments: []*discordgo.MessageAttachment{{Filename: "v.mp4", ContentType: "video/mp4"}},
	}
	if _, ok := ImageSources(m); ok {
		t.Error("含非圖片附件時 ok 應為 false")
	}
	if Eligible(m) {
		t.Error("含非圖片附件時不應 Eligible")
	}
	if _, ok := ImageSources(nil); ok {
		t.Error("nil 訊息 ok 應為 false")
	}
}

func TestEligible_LinkOnly(t *testing.T) {
	cases := map[string]bool{
		example1:                 true,
		"看 https://web.com/page": false,
		"純文字":                    false,
	}
	for content, want := range cases {
		if got := Eligible(&discordgo.Message{Content: content}); got != want {
			t.Errorf("Eligible(%q) = %v，預期 %v", content, got, want)
		}
	}
}

func TestUnresolvedLinks(t *testing.T) {
	const u = "https://example.com/abc"
	cases := []struct {
		name string
		m    *discordgo.Message
		want bool
	}{
		{"無副檔名且沒有 embed", &discordgo.Message{Content: u}, true},
		{"有 URL 相符的 article embed", &discordgo.Message{Content: u, Embeds: []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeArticle, URL: u}}}, false},
		{"有縮圖相符的 embed", &discordgo.Message{Content: u, Embeds: []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: "x", Thumbnail: &discordgo.MessageEmbedThumbnail{URL: u}}}}, false},
		{"只有副檔名圖片網址", &discordgo.Message{Content: "https://a.com/x.png"}, false},
		{"副檔名圖片＋無 embed 連結", &discordgo.Message{Content: "https://a.com/x.png " + u}, true},
		{"<網址> 不會產生預覽", &discordgo.Message{Content: "<" + u + ">"}, false},
		{"訊息關閉預覽", &discordgo.Message{Content: u, Flags: discordgo.MessageFlagsSuppressEmbeds}, false},
		{"純文字", &discordgo.Message{Content: "abc"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := UnresolvedLinks(c.m); got != c.want {
			t.Errorf("%s：UnresolvedLinks = %v，預期 %v", c.name, got, c.want)
		}
	}
}

func TestTextWithoutImageURLs(t *testing.T) {
	cases := map[string]string{
		example1:                          "mygo、我早就知道會這樣了",
		example2:                          "我不一想說得太失禮 你還是放棄吧",
		"https://i.imgur.com/jgoYzsS.jpg": "",
		"看 https://web.com/page 和 https://a.com/x.png": "看 https://web.com/page 和",
		"[這張](https://a.com/x.png) 很好":                 "這張 很好",
		"||https://a.com/x.png|| 劇透":                   "劇透",
		"https://a.com/x.png 兩次 https://a.com/x.png":   "兩次",
		"前 https://a.com/x.png 後":                      "前 後",
		"前\nhttps://a.com/x.png\n後":                    "前\n\n後",
	}
	for content, want := range cases {
		if got := TextWithoutImageURLs(&discordgo.Message{Content: content}); got != want {
			t.Errorf("TextWithoutImageURLs(%q) = %q，預期 %q", content, got, want)
		}
	}
}

// LinkSources 只看文字中的圖片網址（不看附件；含非圖片附件的訊息也回傳）。
func TestLinkSources(t *testing.T) {
	m := &discordgo.Message{
		Content:     "https://a.com/x.png https://web.com/",
		Attachments: []*discordgo.MessageAttachment{{Filename: "v.mp4", ContentType: "video/mp4"}},
	}
	srcs := LinkSources(m)
	if len(srcs) != 1 || srcs[0].URL != "https://a.com/x.png" || srcs[0].Index != 0 {
		t.Errorf("LinkSources = %+v", srcs)
	}
}

// U3：新訊息不把 gifv 當圖片；gifv 網址留在文字中（當一般連結）。
func TestNewMessageSources_ExcludesGIFV(t *testing.T) {
	const g = "https://tenor.com/view/abc"
	m := &discordgo.Message{
		Content: "動圖 " + g + " 圖 https://a.com/x.png",
		Embeds:  []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeGifv, URL: g}},
	}
	all, _ := ImageSources(m)
	if len(all) != 2 {
		t.Fatalf("舊訊息規則應含 gifv：%+v", all)
	}
	srcs, ok := NewMessageSources(m)
	if !ok || len(srcs) != 1 || srcs[0].URL != "https://a.com/x.png" {
		t.Fatalf("新訊息規則應排除 gifv：%+v %v", srcs, ok)
	}
	if got := TextWithoutLinks(m.Content, srcs); got != "動圖 "+g+" 圖" {
		t.Errorf("gifv 網址應留在文字中：%q", got)
	}
}

// HasImagePreview：Discord 對這個網址產生了圖片預覽（只靠副檔名不算）。
func TestHasImagePreview(t *testing.T) {
	const u = "https://a.com/x.jpg"
	cases := []struct {
		name   string
		embeds []*discordgo.MessageEmbed
		want   bool
	}{
		{"沒有預覽", nil, false},
		{"image 且 URL 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: u}}, true},
		{"gifv 且 URL 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeGifv, URL: u}}, true},
		// PM 決定（第三輪把關後）：image／thumbnail 比對只看 type image／gifv 的預覽；article／link 的 og:image 是網頁自己指定的，不算。
		{"article 的 thumbnail 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeArticle, URL: "https://o.com", Thumbnail: &discordgo.MessageEmbedThumbnail{URL: u}}}, false},
		{"link 的 image 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeLink, Image: &discordgo.MessageEmbedImage{URL: u}}}, false},
		{"image 型、URL 不同但 thumbnail 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: "https://o.com/p", Thumbnail: &discordgo.MessageEmbedThumbnail{URL: u}}}, true},
		{"gifv 型、image 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeGifv, URL: "https://o.com/v", Image: &discordgo.MessageEmbedImage{URL: u}}}, true},
		// PM 決定（第二輪把關後）：rich embed 是 bot／webhook 自訂的內容，不算 Discord 產生的預覽。
		{"rich 的 image 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeRich, Image: &discordgo.MessageEmbedImage{URL: u}}}, false},
		{"rich 的 thumbnail 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeRich, Thumbnail: &discordgo.MessageEmbedThumbnail{URL: u}}}, false},
		{"article 且 URL 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeArticle, URL: u}}, false},
		{"link 且 URL 相同", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeLink, URL: u}}, false},
		{"image 但屬於別的網址", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: "https://a.com/y.jpg"}}, false},
	}
	for _, c := range cases {
		if got := HasImagePreview(u, c.embeds); got != c.want {
			t.Errorf("%s：HasImagePreview = %v，預期 %v", c.name, got, c.want)
		}
	}
}
