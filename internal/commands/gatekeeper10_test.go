package commands

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"discordbot/internal/access"

	"github.com/bwmarrin/discordgo"
)

// 把關者（階段 10）補充測試。

// syncAvailable 是並行安全的假可用頻道判定（fakeAvailable 會記錄呼叫者，不是並行安全）。
type syncAvailable struct {
	mu   sync.Mutex
	refs []access.ChannelRef
}

func (a *syncAvailable) Available(ctx context.Context, userID string) ([]access.ChannelRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]access.ChannelRef(nil), a.refs...), nil
}

// 兩個選單幾乎同時送出：兩頁的變更都要保留（新勾選在 store.Update 內計算）。
func TestGK10_ConcurrentMenusBothKept(t *testing.T) {
	for _, preChosen := range []bool{true, false} {
		t.Run(fmt.Sprint("先前已選過=", preChosen), func(t *testing.T) {
			for round := 0; round < 20; round++ {
				e := newPKEnv(t, manyRefs(60)...)
				sa := &syncAvailable{refs: manyRefs(60)}
				e.s.Access = sa
				if preChosen {
					e.choose(t, "c0", "c1", "c30", "c50")
				}
				msg := e.shown(t)
				var wg sync.WaitGroup
				for _, sel := range []struct {
					id   string
					vals []string
				}{
					{SourcesMenuPrefix + ":0", []string{"c2"}},
					{SourcesMenuPrefix + ":1", []string{"c26", "c27"}},
					{SourcesMenuPrefix + ":2", nil},
				} {
					wg.Add(1)
					go func() {
						defer wg.Done()
						e.s.Component(context.Background(), sourcesSelect(pkUser, sel.id, sel.vals, msg))
					}()
				}
				wg.Wait()
				ids, ok := chosen(t, e.st)
				// 60 個頻道 = 3 頁（c0..c24、c25..c49、c50..c59），三個選單各自整頁取代，
				// 所以不論先前勾選為何（c50 在第 2 頁被清掉），結果都是三頁選到的值。
				want := []string{"c2", "c26", "c27"}
				if !ok || !sameSet(ids, want) {
					t.Fatalf("第 %d 輪：勾選 %v，預期 %v", round, ids, want)
				}
			}
		})
	}
}

// 偽造訊息內容（若 Discord 允許）：把別頁的頻道放進「這一頁」。最壞情況只是使用者取消了自己看得到的頻道，
// 等同正常操作；看不到的頻道不會被加入，也不會被移除。
func TestGK10_ForgedPageWithOtherPageChannel(t *testing.T) {
	e := newPKEnv(t, manyRefs(30)...)
	e.choose(t, "c0", "c1", "c26", "secret")
	msg := &discordgo.Message{Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: SourcesMenuPrefix + ":0", Options: []discordgo.SelectMenuOption{
			{Label: "a", Value: "c26"}, {Label: "b", Value: "secret"}, {Label: "c", Value: "other-secret"},
		}},
	}}}}
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"other-secret"}, msg))
	ids, _ := chosen(t, e.st)
	if !sameSet(ids, []string{"c0", "c1", "secret"}) {
		t.Errorf("勾選 %v（c26 可見且在偽造頁中 → 取消；secret 看不到 → 保留；other-secret 不可加入）", ids)
	}
}

// Q47：沒選過時第一次變更以當下 Available 展開，之後新增的來源頻道不自動勾選。
func TestGK10_FirstChangeThenNewSourceNotAutoAdded(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"))
	msg := e.shown(t)
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"c1"}, msg))
	e.access.refs[pkUser] = []access.ChannelRef{ref("1", "c1"), ref("1", "c2"), ref("1", "c3")}
	msg = e.shown(t)
	if got := defaults(menus(t, msg.Components)); !sameSet(got, []string{"c1"}) {
		t.Errorf("新增 c3 後預設勾選 %v，預期只有 c1", got)
	}
}

// 記錄目前行為（待 PM／使用者決定）：沒選過的使用者第一次變更時，若 Available 暫時少了某伺服器
// （成員查詢逾時、失敗或伺服器暫時不在 state），該伺服器的頻道被永久排除在勾選之外。
func TestGK10_TransientMissingServerOnFirstChange(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("2", "d1"))
	msg := e.shown(t)
	e.access.refs[pkUser] = []access.ChannelRef{ref("1", "c1")} // 伺服器 2 暫時查不到
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"c1"}, msg))
	ids, ok := chosen(t, e.st)
	t.Logf("目前行為：chosen=%v ids=%v（d1 是否被排除：%v）", ok, ids, !containsStr(ids, "d1"))
}

// 記錄目前行為：CustomID 不合法（例如 pickch:9）時，沒選過的使用者仍被寫成「已選全部可用頻道」並寫入儲存頻道。
func TestGK10_InvalidMenuIndexStillWrites(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"))
	msg := e.shown(t)
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":9", []string{"c1"}, msg))
	ids, ok := chosen(t, e.st)
	t.Logf("目前行為：chosen=%v ids=%v 寄出儲存訊息 %d 則", ok, ids, len(e.fake.Sent))
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
