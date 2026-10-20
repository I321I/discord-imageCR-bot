package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"discordbot/internal/discordtest"
	"discordbot/internal/snowflake"
)

// gateR2BigData 回傳編碼後超過一則訊息（2000 字）的資料。
func gateR2BigData() *Data {
	d := NewData()
	for i := 0; i < 80; i++ {
		d.AddChannel(fmt.Sprintf("10000000000000%04d", i), fmt.Sprintf("20000000000000%04d", i), "100")
	}
	return d
}

// 把關第二輪：多則訊息的設定，EnsureURLSince 任一則寫入失敗（第一次或第二次寫入的中間）時，
// 記憶體不變、儲存頻道最新完整版本仍是原設定；重啟（重新 Load）後再呼叫會成功，url_since 為新寫入的訊息 ID。
func TestGateR2_EnsureURLSince_MultiPartFailures(t *testing.T) {
	d0 := gateR2BigData()
	parts, err := Encode(d0, "1")
	if err != nil || len(parts) < 2 {
		t.Fatalf("測試資料應拆成多則：%d %v", len(parts), err)
	}
	k := len(parts)
	for failAt := 1; failAt <= 2*k; failAt++ {
		f := discordtest.NewFake()
		addEncoded(t, f, d0, "1")
		s := New(f, storageCh)
		if err := s.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.api = &failNthStoreSend{Fake: f, n: failAt}
		if _, err := s.EnsureURLSince(context.Background()); !errors.Is(err, discordtest.ErrInjected) {
			t.Fatalf("第 %d 則失敗應回錯誤：%v", failAt, err)
		}
		if s.Snapshot().URLSince != "" || len(s.Snapshot().Guilds) != 80 {
			t.Errorf("第 %d 則失敗：記憶體不應改變", failAt)
		}
		// 重啟：重新讀取，最新完整版本仍是原設定。
		s2 := New(f, storageCh)
		if err := s2.Load(context.Background()); err != nil {
			t.Fatalf("第 %d 則失敗後重新讀取失敗：%v", failAt, err)
		}
		if got := s2.Snapshot(); got.URLSince != "" || len(got.Guilds) != 80 {
			t.Errorf("第 %d 則失敗後儲存頻道最新完整版本應是原設定：url_since=%q guilds=%d", failAt, got.URLSince, len(got.Guilds))
		}
		// 重啟後再呼叫成功，且值為這次寫入的第一則訊息 ID（晚於先前所有訊息）。
		before := f.Messages(storageCh)
		at, err := s2.EnsureURLSince(context.Background())
		if err != nil {
			t.Fatalf("重啟後應成功：%v", err)
		}
		for _, m := range before {
			if !snowflake.Less(m.ID, at) {
				t.Errorf("url_since %s 應晚於先前的訊息 %s", at, m.ID)
			}
		}
		s3 := New(f, storageCh)
		if err := s3.Load(context.Background()); err != nil || s3.Snapshot().URLSince != at || len(s3.Snapshot().Guilds) != 80 {
			t.Errorf("第 %d 則失敗後重試：存回不正確 %q %v", failAt, s3.Snapshot().URLSince, err)
		}
	}
}

// 把關第二輪：EnsureURLSince 與 Update、Reload 同時進行（-race），結束後 url_since 與其他修改都保留。
func TestGateR2_EnsureURLSince_ConcurrentWithUpdateReload(t *testing.T) {
	for round := 0; round < 20; round++ {
		f := discordtest.NewFake()
		s := New(f, storageCh)
		if err := s.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var at string
		wg.Add(3)
		go func() {
			defer wg.Done()
			v, err := s.EnsureURLSince(context.Background())
			if err != nil {
				t.Error(err)
			}
			at = v
		}()
		go func() {
			defer wg.Done()
			if err := s.Update(context.Background(), func(d *Data) error { d.AddChannel("g", "c", "1"); return nil }); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.Reload(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		d := s.Snapshot()
		if d.URLSince == "" || d.URLSince != at {
			t.Errorf("第 %d 輪：url_since 應保留 %q，得到 %q", round, at, d.URLSince)
		}
		if _, _, ok := d.SourceChannel("c"); !ok {
			t.Errorf("第 %d 輪：Update 的修改應保留", round)
		}
		s2 := New(f, storageCh)
		if err := s2.Load(context.Background()); err != nil || s2.Snapshot().URLSince != at {
			t.Errorf("第 %d 輪：儲存頻道最新版本 url_since=%q，預期 %q（%v）", round, s2.Snapshot().URLSince, at, err)
		}
	}
}
