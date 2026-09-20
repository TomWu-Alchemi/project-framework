package util

import (
	"slices"
	"testing"
)

// TestSliceRemoveDuplicates_Cases 覆盖 nil / 空 / 单元素 / 多元素含重复 / 全重复，
// 并锁定"保留首次出现顺序"的去重语义（改动前后一致）。
func TestSliceRemoveDuplicates_Cases(t *testing.T) {
	tests := []struct {
		name string
		in   []int
		want []int
	}{
		{name: "nil 输入", in: nil, want: nil},
		{name: "空切片（非 nil）", in: []int{}, want: nil},
		{name: "单元素", in: []int{42}, want: []int{42}},
		{name: "多元素含重复", in: []int{3, 1, 3, 2, 1, 2, 4}, want: []int{3, 1, 2, 4}},
		{name: "全部重复", in: []int{7, 7, 7, 7}, want: []int{7}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SliceRemoveDuplicates(tt.in)
			// slices.Equal 把 nil 与空切片视为相等：本表只断言"内容与长度"，
			// 不绑定 nil/空的差异（nil 入参的返回语义见下方专门用例）。
			if !slices.Equal(got, tt.want) {
				t.Fatalf("SliceRemoveDuplicates(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestSliceRemoveDuplicates_AlwaysReturnsNewSlice 锁定 P3-6 的已批准行为变化：
// len <= 1 时也必须返回新切片，不得把入参原样返回
// （否则调用方修改返回值会串改原数据）。
func TestSliceRemoveDuplicates_AlwaysReturnsNewSlice(t *testing.T) {
	t.Run("单元素：不共享底层数组", func(t *testing.T) {
		in := []int{42}
		got := SliceRemoveDuplicates(in)
		if !slices.Equal(got, in) {
			t.Fatalf("got=%v want=%v", got, in)
		}
		if &got[0] == &in[0] {
			t.Fatal("len<=1 时应返回新切片，但结果与入参共享底层数组")
		}
		got[0] = 99 // 行为级断言：写返回值不得影响入参
		if in[0] != 42 {
			t.Fatalf("修改返回值影响了入参：in=%v", in)
		}
	})

	t.Run("空切片：不共享底层数组（特殊处理：空切片没有元素可取地址）", func(t *testing.T) {
		// 关键：给入参多余容量（cap=8）。空切片唯一的"别名观测途径"是越过 len、
		// 依赖 cap 的 reslice / append，因此必须留出容量才能把"共享底层数组"变成可观测现象；
		// 同时这也使本用例能检出 P3-6 修复前的行为（旧实现在 len<=1 时直接返回入参，cap=8）。
		in := make([]int, 0, 8)
		got := SliceRemoveDuplicates(in)
		if len(got) != 0 {
			t.Fatalf("got len=%d, want 0", len(got))
		}
		// 对返回值 append：若结果与入参共享底层数组（旧行为），该写入会落到 in 的
		// 底层数组首槽，可通过 in[:1]（cap 允许，合法 reslice）读回；独立切片则读回零值。
		_ = append(got, 99)
		if v := in[:1][0]; v != 0 {
			t.Fatalf("对返回值 append 污染了入参底层数组：in 底层首槽=%d", v)
		}
	})

	t.Run("nil 输入：返回 nil（nil 语义不变）", func(t *testing.T) {
		var in []int
		got := SliceRemoveDuplicates(in)
		if len(got) != 0 {
			t.Fatalf("got len=%d, want 0", len(got))
		}
		// slices.Clone(nil) == nil。显式锁定该语义，避免后续换成其他写法时
		// 把 nil 变成空切片（会改变 JSON 序列化：null → []）。
		if got != nil {
			t.Fatalf("nil 入参应返回 nil，实际返回空切片 (cap=%d)", cap(got))
		}
	})

	t.Run("多元素：不共享底层数组", func(t *testing.T) {
		in := []int{1, 2, 1, 3}
		got := SliceRemoveDuplicates(in)
		if !slices.Equal(got, []int{1, 2, 3}) {
			t.Fatalf("got=%v", got)
		}
		got[0] = 99
		if in[0] != 1 {
			t.Fatalf("修改返回值影响了入参：in=%v", in)
		}
	})
}
