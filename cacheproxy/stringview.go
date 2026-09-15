package cacheproxy

import "time"

type StringView struct {
	Ctime           time.Time `json:"ctime"`
	NeedFastRequery bool      `json:"need_fast_requery"`
	IsNil           bool      `json:"is_nil"`
	Data            string    `json:"data"`
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

func (v StringView) CheckNil() bool {
	return v.IsNil
}

func (v StringView) ByteSlice() []byte {
	return []byte(v.Data)
}

func (v StringView) GetTime() time.Time {
	return v.Ctime
}
