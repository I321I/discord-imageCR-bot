package commands

import "sync"

// messageLocks 是以訊息 ID 為 key 的鎖：同一則圖片訊息的「讀最新內容 → 合併 → 寫回」互斥，
// 避免編輯表單與重新辨識互相覆蓋。以引用計數管理，沒人使用時就從 map 移除，不會累積。零值可用。
type messageLocks struct {
	mu    sync.Mutex
	locks map[string]*refLock
}

type refLock struct {
	mu   sync.Mutex
	refs int // 持有或等待中的數量（受 messageLocks.mu 保護）
}

// lock 取得 key 的鎖，回傳釋放函式。
func (l *messageLocks) lock(key string) (unlock func()) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*refLock{}
	}
	r := l.locks[key]
	if r == nil {
		r = &refLock{}
		l.locks[key] = r
	}
	r.refs++
	l.mu.Unlock()

	r.mu.Lock()
	return func() {
		r.mu.Unlock()
		l.mu.Lock()
		r.refs--
		if r.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}
