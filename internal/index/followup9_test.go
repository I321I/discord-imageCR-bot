package index

import (
	"fmt"
	"math/rand"
	"testing"

	"discordbot/internal/imagemsg"

	"github.com/bwmarrin/discordgo"
)

// 階段 9 收尾（PM 決定 5）：舊圖片訊息的張數與編號只計圖片附件。
func TestPutLegacy_NumbersOnlyImages(t *testing.T) {
	x := New()
	m := &discordgo.Message{ID: "1", ChannelID: "c", Content: "舊的", Author: &discordgo.User{ID: "u1"},
		Attachments: []*discordgo.MessageAttachment{
			{ID: "p0", Filename: "0.png", ContentType: "image/png"},
			{ID: "z1", Filename: "1.zip", ContentType: "application/zip"},
			{ID: "p2", Filename: "2.png", ContentType: "image/png"},
		}}
	x.PutLegacy("g", m)
	for _, c := range []struct{ att, number int }{{0, 1}, {2, 2}} {
		e, ok := x.Get(Key{ChannelID: "c", MessageID: "1", Attachment: c.att})
		if !ok {
			t.Fatalf("附件 %d 應收錄", c.att)
		}
		if e.AttachmentCount != 2 || e.Number != c.number {
			t.Errorf("附件 %d：張數 %d、編號 %d，預期 2、%d", c.att, e.AttachmentCount, e.Number, c.number)
		}
	}
}

// 階段 9 收尾（PM 決定 2）：索引記錄附件 ID。
func TestPut_StoresAttachmentID(t *testing.T) {
	x := New()
	im := imageMsg("c", "1", imagemsg.Fields{Work: "芙莉蓮"}, "up")
	im.Attachments[0].ID = "att-img"
	x.PutImageMessage("g", im)
	if e, _ := x.Get(Key{ChannelID: "c", MessageID: "1"}); e.AttachmentID != "att-img" || e.Number != 1 || e.AttachmentCount != 1 {
		t.Errorf("圖片訊息 %+v", e)
	}
	lm := legacyMsg("c", "2", "舊的", 2)
	lm.Attachments[0].ID, lm.Attachments[1].ID = "L0", "L1"
	x.PutLegacy("g", lm)
	if e, _ := x.Get(Key{ChannelID: "c", MessageID: "2", Attachment: 1}); e.AttachmentID != "L1" {
		t.Errorf("舊圖片訊息 %+v", e)
	}
}

// 階段 9 收尾（PM 決定 1）：只移除一則訊息中的一張圖；最後一張移除後整則消失。
func TestRemoveAttachment(t *testing.T) {
	x := New()
	x.PutLegacy("g", legacyMsg("c", "1", "舊的", 2))
	x.RemoveAttachment(Key{ChannelID: "c", MessageID: "1", Attachment: 1})
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "1", Attachment: 1}); ok {
		t.Error("附件 1 應移除")
	}
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "1", Attachment: 0}); !ok {
		t.Error("附件 0 應保留")
	}
	x.RemoveAttachment(Key{ChannelID: "c", MessageID: "1", Attachment: 0})
	if got := x.Search("", allow("c"), 0); len(got) != 0 {
		t.Errorf("應全部移除：%v", keys(got))
	}
	x.RemoveAttachment(Key{ChannelID: "none", MessageID: "1"}) // 不存在：不 panic
}

// 階段 9 收尾（PM 決定 4）：搜尋不在每次查詢時重新正規化文字，也不為全部符合項目排序——
// 配置次數與項目數無關。
func TestSearch_AllocationsIndependentOfSize(t *testing.T) {
	x := New()
	for n := 0; n < 5000; n++ {
		x.PutImageMessage("g", imageMsg("c", fmt.Sprint(1000+n), imagemsg.Fields{Work: fmt.Sprintf("Frieren 作品 %d", n), Text: "Hello World"}, "up"))
	}
	allowed := allow("c")
	allocs := testing.AllocsPerRun(5, func() { x.Search("frieren", allowed, 25) })
	if allocs > 200 {
		t.Errorf("每次搜尋配置 %.0f 次，應與項目數無關（≤ 200）", allocs)
	}
}

// 只保留前 N 名的結果必須與完整排序後取前 N 名相同。
func TestSearch_TopNSameAsFullSort(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	x := New()
	words := []string{"芙莉蓮", "費倫", "Stark", "魔法", "勇者"}
	for n := 0; n < 600; n++ {
		ch := fmt.Sprint("c", r.Intn(3))
		id := fmt.Sprint(r.Intn(1 << 30))
		w := func() string { return words[r.Intn(len(words))] }
		if r.Intn(3) == 0 {
			x.PutLegacy("g", legacyMsg(ch, id, w()+" "+w(), 1+r.Intn(3)))
		} else {
			x.PutImageMessage("g", imageMsg(ch, id, imagemsg.Fields{Work: w(), Text: w(), Keywords: w()}, "up"))
		}
	}
	allowed := allow("c0", "c1", "c2")
	for _, q := range []string{"", "芙莉蓮", "stark 魔法", "勇者"} {
		all := x.Search(q, allowed, 0)
		for _, limit := range []int{1, 7, 25} {
			got := x.Search(q, allowed, limit)
			want := all
			if len(want) > limit {
				want = want[:limit]
			}
			if fmt.Sprint(keys(got)) != fmt.Sprint(keys(want)) {
				t.Errorf("查詢 %q 前 %d 名：\n得到 %v\n預期 %v", q, limit, keys(got), keys(want))
			}
		}
	}
}

// 舊圖片訊息改字後，搜尋用的是新文字（快取的正規化文字要一起更新）。
func TestUpdateLegacyText_RefreshesNormalizedText(t *testing.T) {
	x := New()
	x.PutLegacy("g", legacyMsg("c", "1", "舊的文字", 1))
	x.UpdateLegacyText("c", "1", "NEW Text")
	if got := x.Search("new", allow("c"), 0); len(got) != 1 {
		t.Errorf("應以新文字搜尋到：%v", keys(got))
	}
	if got := x.Search("舊的", allow("c"), 0); len(got) != 0 {
		t.Errorf("舊文字不應再符合：%v", keys(got))
	}
}
