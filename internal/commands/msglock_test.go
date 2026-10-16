package commands

import (
	"sync"
	"testing"
	"time"
)

// 同一個 key 互斥；不同 key 互不影響；用完清掉、不累積。
func TestMessageLocks(t *testing.T) {
	var l messageLocks
	unlockA := l.lock("a")
	unlockB := l.lock("b") // 不同 key 不會被擋住
	got := make(chan struct{})
	go func() {
		u := l.lock("a")
		close(got)
		u()
	}()
	select {
	case <-got:
		t.Fatal("同一個 key 應互斥")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("釋放後應取得鎖")
	}
	unlockB()
	if n := l.size(); n != 0 {
		t.Errorf("用完應清掉，剩 %d 個", n)
	}
}

func TestMessageLocks_ConcurrentNoLeak(t *testing.T) {
	var l messageLocks
	var wg sync.WaitGroup
	counter := map[string]*int{"x": new(int), "y": new(int), "z": new(int)} // 只讀 map，各 key 的計數由該 key 的鎖保護
	for i := 0; i < 100; i++ {
		key := []string{"x", "y", "z"}[i%3]
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := l.lock(key)
			*counter[key]++ // -race 檢查互斥
			u()
		}()
	}
	wg.Wait()
	if *counter["x"]+*counter["y"]+*counter["z"] != 100 {
		t.Errorf("計數 %v", counter)
	}
	if n := l.size(); n != 0 {
		t.Errorf("用完應清掉，剩 %d 個", n)
	}
}

// size 回傳目前登記中的 key 數。
func (l *messageLocks) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}
