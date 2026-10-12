package store

import (
	"slices"
	"testing"
)

func TestAddChannel_Duplicate(t *testing.T) {
	d := NewData()
	if !d.AddChannel("g", "c", "100") {
		t.Fatal("第一次新增應回 true")
	}
	if d.AddChannel("g", "c", "200") {
		t.Fatal("重複新增應回 false")
	}
	if _, setAt, _ := d.SourceChannel("c"); setAt != "100" {
		t.Errorf("重複新增不應改 setAt，得到 %s", setAt)
	}
}

func TestRemoveChannel_AlsoClearsUserSelections(t *testing.T) {
	d := NewData()
	d.AddChannel("g", "c1", "1")
	d.AddChannel("g", "c2", "2")
	d.SetUserChannels("u", []string{"c1", "c2"})
	if !d.RemoveChannel("g", "c1") {
		t.Fatal("移除存在的頻道應回 true")
	}
	if d.RemoveChannel("g", "c1") {
		t.Fatal("再次移除應回 false")
	}
	if ids, chosen := d.UserChannels("u"); !chosen || !slices.Equal(ids, []string{"c2"}) {
		t.Errorf("使用者勾選應剩 c2：%v %v", ids, chosen)
	}
	if got := d.Channels("g"); !slices.Equal(got, []string{"c2"}) {
		t.Errorf("Channels 應剩 c2：%v", got)
	}
}

func TestRemoveChannel_SelectionBecomesEmptyMeansNeverChosen(t *testing.T) {
	d := NewData()
	d.AddChannel("g", "c1", "1")
	d.AddChannel("g", "c2", "2")
	d.SetUserChannels("u", []string{"c1"})
	d.SetUserChannels("chose-none", []string{})
	d.RemoveChannel("g", "c1")
	if ids, chosen := d.UserChannels("u"); chosen {
		t.Errorf("勾選因頻道被移除而變成 0 個時應視為沒選過：%v %v", ids, chosen)
	}
	if ids, chosen := d.UserChannels("chose-none"); !chosen || len(ids) != 0 {
		t.Errorf("使用者自己選 0 個應保持「選了 0 個」：%v %v", ids, chosen)
	}
}

func TestRemoveChannel_OnlyGivenGuild(t *testing.T) {
	d := NewData()
	d.AddChannel("g1", "c", "1")
	if d.RemoveChannel("g2", "c") {
		t.Fatal("不屬於 g2 的頻道不應被移除")
	}
	if _, _, ok := d.SourceChannel("c"); !ok {
		t.Error("頻道應仍存在")
	}
}

func TestRemoveChannel_DropsEmptyGuild(t *testing.T) {
	d := NewData()
	d.AddChannel("g", "c", "1")
	d.RemoveChannel("g", "c")
	if _, ok := d.Guilds["g"]; ok {
		t.Error("沒有頻道的伺服器應從 Guilds 移除")
	}
}

func TestChannels_SortedBySetAt(t *testing.T) {
	d := NewData()
	d.AddChannel("g", "a", "300")
	d.AddChannel("g", "b", "20") // 位數較少，數值較小
	d.AddChannel("g", "c", "100")
	if got := d.Channels("g"); !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Errorf("應依 setAt 數值排序：%v", got)
	}
	if got := d.Channels("none"); len(got) != 0 {
		t.Errorf("不存在的伺服器應回空：%v", got)
	}
}

func TestUserChannels_NeverChosenVsEmpty(t *testing.T) {
	d := NewData()
	if ids, chosen := d.UserChannels("u"); chosen || ids != nil {
		t.Errorf("沒選過應為 chosen=false：%v %v", ids, chosen)
	}
	d.SetUserChannels("u", nil)
	ids, chosen := d.UserChannels("u")
	if !chosen || ids == nil || len(ids) != 0 {
		t.Errorf("選了空集合應為 chosen=true、ids=[]：%v %v", ids, chosen)
	}
}

func TestSourceChannel(t *testing.T) {
	d := NewData()
	d.AddChannel("g", "c", "123")
	g, setAt, ok := d.SourceChannel("c")
	if !ok || g != "g" || setAt != "123" {
		t.Errorf("得到 %s %s %v", g, setAt, ok)
	}
	if _, _, ok := d.SourceChannel("x"); ok {
		t.Error("未設定的頻道應回 ok=false")
	}
}

func TestClone_Independent(t *testing.T) {
	d := NewData()
	d.AddChannel("g", "c", "1")
	d.SetUserChannels("u", []string{"c"})
	c := d.Clone()
	c.AddChannel("g", "c2", "2")
	c.SetUserChannels("u", []string{"c2"})
	c.UserSelections["u"][0] = "zzz"
	if len(d.Channels("g")) != 1 {
		t.Error("Clone 後修改不應影響原資料（頻道）")
	}
	if ids, _ := d.UserChannels("u"); ids[0] != "c" {
		t.Error("Clone 後修改不應影響原資料（勾選）")
	}
}
