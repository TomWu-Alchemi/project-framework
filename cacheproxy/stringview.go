package cacheproxy

import "time"

type StringView struct {
	Ctime           time.Time `json:"ctime"`
	NeedFastRequery bool      `json:"need_fast_requery"`

	// IsNil（F-34 标记 deprecated）：当前不参与判定语义——写入侧（setData）恒为
	// false，读取侧的 miss 语义由 exist=false 表达，空值 TTL 按 len(Data)==0 判定。
	// 计划下个主版本移除，勿在新代码中依赖。
	IsNil bool   `json:"is_nil"`
	Data  string `json:"data"`
}

func (v StringView) IsExpire(normalOffset time.Duration, fastOffset time.Duration) bool {
	if v.Ctime.IsZero() {
		return true
	}
	offset := normalOffset
	if v.NeedFastRequery {
		offset = durationOrDefault(fastOffset, normalOffset)
	}
	offset = durationOrDefault(offset, defaultRefreshTime)
	return v.Ctime.Add(offset).Before(time.Now())
}

func (v StringView) Len() int {
	return len(v.Data)
}

func (v StringView) String() string {
	return v.Data
}

// CheckNil（F-34 标记 deprecated）：见 IsNil 字段说明——该值对调用方永远不可用，
// 计划下个主版本随 IsNil 一并移除。
func (v StringView) CheckNil() bool {
	return v.IsNil
}

func (v StringView) ByteSlice() []byte {
	return []byte(v.Data)
}

func (v StringView) GetTime() time.Time {
	return v.Ctime
}
