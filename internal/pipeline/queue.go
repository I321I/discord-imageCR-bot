package pipeline

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"

	"discordbot/internal/recognize"
	"discordbot/internal/snowflake"

	"github.com/bwmarrin/discordgo"
)

// job 是佇列中的一則原訊息。只存訊息參照，輪到時才下載附件。
type job struct {
	ctx     context.Context
	guildID string
	m       *discordgo.Message
	refetch bool // 補處理：輪到時重新取訊息，已刪除或帶 ❌ 就略過
	strict  bool // Reprocess：被略過時回傳 ErrAlreadyQueued／ErrNotSource（其他來源回 nil）
	done    chan error
}

// Reprocess 被略過時的結果（一般 Enqueue、EnqueueBackfill 被略過時回 nil）。
var (
	// ErrAlreadyQueued 表示同一則訊息已在佇列中或處理中。
	ErrAlreadyQueued = errors.New("message already queued")
	// ErrNotSource 表示輪到時頻道已不是來源頻道。
	ErrNotSource = errors.New("channel is no longer a source")
)

// Enqueue 把一則來源頻道的原訊息排入佇列（決定 7：同一時間只處理一則），立即在原訊息加 ⏳，
// 回傳處理完成時收到結果的 channel。佇列依原訊息 ID（snowflake 數值）由舊到新處理（決定 9）。
// 同一則訊息已在佇列或處理中時略過，channel 直接收到 nil。
// 輪到時 ctx 已結束（關機）：不下載、不發、不刪、不加 ❌，只移除 ⏳。
func (p *Pipeline) Enqueue(ctx context.Context, guildID string, m *discordgo.Message) <-chan error {
	return p.enqueue(ctx, guildID, m, false, false)
}

// EnqueueBackfill 與 Enqueue 相同，但輪到時先重新取得訊息（取得最新附件網址）：
// 訊息已被刪除，或帶有 bot 的 ❌（使用者決定 10）時略過並移除 ⏳。用於啟動補處理。
func (p *Pipeline) EnqueueBackfill(ctx context.Context, guildID string, m *discordgo.Message) <-chan error {
	return p.enqueue(ctx, guildID, m, true, false)
}

// Reprocess 重新處理帶 bot ❌ 的原訊息（對自己發的原訊息使用「重新辨識」）：先移除 ❌，再同 Enqueue 排入佇列
// （下載、辨識、發出、全部成功才刪原訊息；失敗時重新加 ❌）。辨識以優先排隊（Q62）。
// 已在佇列中或處理中 → ErrAlreadyQueued；輪到時頻道已非來源 → ErrNotSource。
// 部分成功過的訊息會重複發出已發過的圖（使用者決定，由使用者手動刪除）。
// ctx 會跟著訊息留在佇列中，必須是 bot 關機才取消的 ctx。
func (p *Pipeline) Reprocess(ctx context.Context, guildID string, m *discordgo.Message) <-chan error {
	p.unreact(m, reactionFailed)
	return p.enqueue(recognize.WithPriority(ctx), guildID, m, false, true)
}

func (p *Pipeline) enqueue(ctx context.Context, guildID string, m *discordgo.Message, refetch, strict bool) <-chan error {
	done := make(chan error, 1)
	p.qmu.Lock()
	if p.pending == nil {
		p.pending = map[string]bool{}
	}
	if p.pending[m.ID] {
		p.qmu.Unlock()
		if strict {
			done <- ErrAlreadyQueued
		} else {
			done <- nil
		}
		return done
	}
	p.pending[m.ID] = true
	p.qmu.Unlock()

	p.react(m, reactionProcessing)

	j := &job{ctx: ctx, guildID: guildID, m: m, refetch: refetch, strict: strict, done: done}
	p.qmu.Lock()
	i := sort.Search(len(p.jobs), func(i int) bool { return snowflake.Less(m.ID, p.jobs[i].m.ID) })
	p.jobs = append(p.jobs, nil)
	copy(p.jobs[i+1:], p.jobs[i:])
	p.jobs[i] = j
	if !p.draining {
		p.draining = true
		go p.drain()
	}
	p.qmu.Unlock()
	return done
}

// drain 依序處理佇列，佇列空了就結束（下次 enqueue 再啟動）。
func (p *Pipeline) drain() {
	for {
		p.qmu.Lock()
		if len(p.jobs) == 0 {
			p.draining = false
			p.qmu.Unlock()
			return
		}
		j := p.jobs[0]
		p.jobs[0] = nil
		p.jobs = p.jobs[1:]
		p.qmu.Unlock()

		err := p.run(j)

		p.qmu.Lock()
		delete(p.pending, j.m.ID)
		p.qmu.Unlock()
		j.done <- err
	}
}

// run 處理輪到的一則訊息（⏳ 已在 enqueue 時加上）。
func (p *Pipeline) run(j *job) error {
	m := j.m
	if err := j.ctx.Err(); err != nil {
		p.unreact(m, reactionProcessing)
		return err
	}
	if p.Store != nil {
		if _, _, ok := p.Store.Snapshot().SourceChannel(m.ChannelID); !ok {
			log.Printf("訊息 %s/%s 排隊期間頻道已不是來源頻道，不處理", m.ChannelID, m.ID)
			p.unreact(m, reactionProcessing)
			if j.strict {
				return ErrNotSource
			}
			return nil
		}
	}
	if j.refetch {
		fresh, err := p.API.ChannelMessage(m.ChannelID, m.ID)
		if err != nil {
			p.unreact(m, reactionProcessing)
			var rest *discordgo.RESTError
			if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound {
				return nil // 已被刪除（例如即時事件已處理完）
			}
			return err
		}
		if HasOwnReaction(fresh, reactionFailed) {
			p.unreact(m, reactionProcessing)
			return nil
		}
		m = fresh
	}
	err := p.process(j.ctx, j.guildID, m)
	if err == nil {
		return nil
	}
	p.unreact(m, reactionProcessing)
	if j.ctx.Err() == nil {
		p.react(m, reactionFailed)
	}
	return err
}

// HasOwnReaction 回傳訊息上是否有 bot 自己加的 emoji 反應。
func HasOwnReaction(m *discordgo.Message, emoji string) bool {
	for _, r := range m.Reactions {
		if r != nil && r.Me && r.Emoji != nil && r.Emoji.Name == emoji {
			return true
		}
	}
	return false
}
