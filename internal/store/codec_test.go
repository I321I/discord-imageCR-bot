package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// toMessages 把 Encode 的結果（依發送順序）轉成「由新到舊」的訊息列表。
func toMessages(contents ...[]string) []*discordgo.Message {
	var all []string
	for _, c := range contents {
		all = append(all, c...)
	}
	var out []*discordgo.Message
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, &discordgo.Message{ID: fmt.Sprint(i + 1), Content: all[i]})
	}
	return out
}

func sampleData() *Data {
	d := NewData()
	d.AddChannel("g1", "c1", "100")
	d.AddChannel("g2", "c2", "200")
	d.SetUserChannels("u1", []string{"c1"})
	d.SetUserChannels("u2", []string{})
	return d
}

func TestEncode_SingleMessage(t *testing.T) {
	msgs, err := Encode(sampleData(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("小資料應只有 1 則，得到 %d", len(msgs))
	}
	if !strings.HasPrefix(msgs[0], "imgbot-store v1 1/1\n") {
		t.Errorf("第一行格式不符：%q", msgs[0])
	}
}

func bigData() *Data {
	d := NewData()
	for i := 0; i < 300; i++ {
		d.AddChannel("123456789012345678", fmt.Sprintf("1%017d", i), fmt.Sprintf("9%017d", i))
	}
	return d
}

func TestEncode_SplitsOver2000(t *testing.T) {
	msgs, err := Encode(bigData(), "v9")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 2 {
		t.Fatalf("應拆成多則，得到 %d", len(msgs))
	}
	for i, m := range msgs {
		if n := utf8.RuneCountInString(m); n > 2000 {
			t.Errorf("第 %d 則有 %d 字，超過 2000", i+1, n)
		}
		want := fmt.Sprintf("imgbot-store v9 %d/%d\n", i+1, len(msgs))
		if !strings.HasPrefix(m, want) {
			t.Errorf("第 %d 則標號不符：%q", i+1, strings.SplitN(m, "\n", 2)[0])
		}
	}
}

func TestDecodeLatest_RoundTrip(t *testing.T) {
	for name, d := range map[string]*Data{"small": sampleData(), "big": bigData()} {
		msgs, err := Encode(d, "v1")
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeLatest(toMessages(msgs))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, d) {
			t.Errorf("%s: round trip 不一致\n得到 %+v\n預期 %+v", name, got, d)
		}
	}
}

func TestDecodeLatest_PicksNewest(t *testing.T) {
	old := sampleData()
	newer := sampleData()
	newer.AddChannel("g3", "c3", "300")
	a, _ := Encode(old, "v1")
	b, _ := Encode(newer, "v2")
	got, err := DecodeLatest(toMessages(a, b))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, newer) {
		t.Errorf("應取最新版本")
	}
}

func TestDecodeLatest_SkipsIncompleteNewest(t *testing.T) {
	old := sampleData()
	a, _ := Encode(old, "v1")
	b, _ := Encode(bigData(), "v2")
	b = append(b[:1:1], b[2:]...) // 缺第 2 段
	got, err := DecodeLatest(toMessages(a, b))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, old) {
		t.Errorf("應退回上一個完整版本")
	}
}

func TestDecodeLatest_IgnoresOtherMessages(t *testing.T) {
	a, _ := Encode(sampleData(), "v1")
	msgs := toMessages([]string{"hello"}, a, []string{"imgbot-store 壞掉的標頭", "其他訊息"})
	got, err := DecodeLatest(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sampleData()) {
		t.Errorf("應忽略非 store 訊息")
	}
}

func TestDecodeLatest_NoData(t *testing.T) {
	if _, err := DecodeLatest(nil); !errors.Is(err, ErrNoData) {
		t.Errorf("空頻道應回 ErrNoData，得到 %v", err)
	}
	if _, err := DecodeLatest(toMessages([]string{"hi"})); !errors.Is(err, ErrNoData) {
		t.Errorf("沒有 store 訊息應回 ErrNoData，得到 %v", err)
	}
}

func TestDecodeLatest_NoCompleteVersion(t *testing.T) {
	b, _ := Encode(bigData(), "v2")
	_, err := DecodeLatest(toMessages(b[1:]))
	if !errors.Is(err, ErrNoCompleteVersion) {
		t.Errorf("應回 ErrNoCompleteVersion，得到 %v", err)
	}
}

func TestDecodeLatest_UnknownFormatVersion(t *testing.T) {
	d := sampleData()
	d.FormatVersion = 99
	a, _ := Encode(d, "v1")
	_, err := DecodeLatest(toMessages(a))
	if err == nil || errors.Is(err, ErrNoData) || errors.Is(err, ErrNoCompleteVersion) {
		t.Errorf("未知格式版本應回其他錯誤，得到 %v", err)
	}
}
