package index

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"discordbot/internal/imagemsg"

	"github.com/bwmarrin/discordgo"
)

// 把關者（階段 9 收尾）補充測試。

// 同一訊息 ID 出現在多個頻道、一則多張圖、limit 大於／等於符合數：前 N 名與完整排序一致。
func TestGK10_TopNTieBreakers(t *testing.T) {
	x := New()
	for _, ch := range []string{"c2", "c1", "c3"} {
		x.PutLegacy("g", legacyMsg(ch, "100", "魔法 勇者", 3))
		x.PutLegacy("g", legacyMsg(ch, "200", "魔法", 2))
		x.PutImageMessage("g", imageMsg(ch, "150", imagemsg.Fields{Work: "魔法"}, "u"))
		x.PutImageMessage("g", imageMsg(ch, "50", imagemsg.Fields{Text: "魔法"}, "u"))
	}
	allowed := allow("c1", "c2", "c3")
	for _, q := range []string{"", "魔法", "勇者"} {
		all := x.Search(q, allowed, 0)
		for limit := 1; limit <= len(all)+3; limit++ {
			got := x.Search(q, allowed, limit)
			want := all[:min(limit, len(all))]
			if fmt.Sprint(keys(got)) != fmt.Sprint(keys(want)) {
				t.Fatalf("查詢 %q 前 %d 名：\n得到 %v\n預期 %v", q, limit, keys(got), keys(want))
			}
		}
	}
	// 完整排序本身：圖片訊息（作品）> 圖片文字 > 舊訊息；同級由新到舊，同訊息 ID 依頻道 ID、再依附件序號。
	got := keys(x.Search("魔法", allowed, 0))
	want := []string{
		"c1:150:0", "c2:150:0", "c3:150:0",
		"c1:50:0", "c2:50:0", "c3:50:0",
		"c1:200:0", "c1:200:1", "c2:200:0", "c2:200:1", "c3:200:0", "c3:200:1",
		"c1:100:0", "c1:100:1", "c1:100:2", "c2:100:0", "c2:100:1", "c2:100:2", "c3:100:0", "c3:100:1", "c3:100:2",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("排序：\n得到 %v\n預期 %v", got, want)
	}
}

// RemoveAttachment 之後，剩下的圖保留原本的編號（第幾張圖片），ID 也不變。
func TestGK10_RemoveAttachmentKeepsNumbers(t *testing.T) {
	x := New()
	m := legacyMsg("c", "1", "文字", 3)
	// 第 0 個附件不是圖片：圖片在位置 1、2、3。
	m.Attachments = append([]*discordgo.MessageAttachment{{ID: "txt", Filename: "a.txt", ContentType: "text/plain"}}, m.Attachments...)
	for i, a := range m.Attachments {
		a.ID = fmt.Sprint("a", i)
	}
	x.PutLegacy("g", m)
	x.RemoveAttachment(Key{ChannelID: "c", MessageID: "1", Attachment: 2})
	got := x.Search("", allow("c"), 0)
	if len(got) != 2 {
		t.Fatalf("剩 %d 筆：%v", len(got), keys(got))
	}
	for _, e := range got {
		wantNum := e.Key.Attachment // 位置 1 → 第 1 張、位置 3 → 第 3 張
		if e.Number != wantNum || e.AttachmentID != fmt.Sprint("a", e.Key.Attachment) {
			t.Errorf("%v：Number=%d AttachmentID=%q", e.Key, e.Number, e.AttachmentID)
		}
		t.Logf("%v：AttachmentCount=%d（移除後未重算）", e.Key, e.AttachmentCount)
	}
	// 移除不存在的附件：不動。
	x.RemoveAttachment(Key{ChannelID: "c", MessageID: "1", Attachment: 9})
	if n := len(x.Search("", allow("c"), 0)); n != 2 {
		t.Errorf("移除不存在的附件後剩 %d 筆", n)
	}
}

// 搜尋（持讀鎖期間回傳的是指標，最後才複製）與寫入同時進行：-race 下不得有資料競爭，結果不得混入半更新的資料。
func TestGK10_SearchConcurrentWithWrites(t *testing.T) {
	x := New()
	for n := 0; n < 200; n++ {
		x.PutLegacy("g", legacyMsg("c", fmt.Sprint(1000+n), "芙莉蓮 魔法", 3))
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprint(1000 + r.Intn(200))
				switch r.Intn(4) {
				case 0:
					x.RemoveAttachment(Key{ChannelID: "c", MessageID: id, Attachment: r.Intn(3)})
				case 1:
					x.UpdateLegacyText("c", id, "芙莉蓮 改")
				case 2:
					x.PutLegacy("g", legacyMsg("c", id, "芙莉蓮 魔法", 3))
				default:
					x.RemoveMessage("c", id)
				}
			}
		}(w)
	}
	for n := 0; n < 300; n++ {
		for _, e := range x.Search("芙莉蓮", allow("c"), 25) {
			if e.Key.ChannelID != "c" || e.Kind != KindLegacy {
				t.Fatalf("結果異常：%+v", e)
			}
		}
	}
	close(stop)
	wg.Wait()
}
