package util

import "slices"

// SliceRemoveDuplicates 对 comparable 切片去重，并**始终返回一个新切片**（P3-6）。
//
// 行为约定：
//   - 保留元素首次出现的相对顺序（与改动前一致）；
//   - 返回值与入参不共享底层数组：len <= 1 时也做拷贝（改动前直接返回入参本身，
//     属 P3-6 的已批准行为变化），调用方修改返回值不会影响入参；
//   - nil 入参返回 nil（保留 nil 语义，避免改变 JSON `null` 序列化等可观测行为）；
//     非 nil 空切片返回非 nil 空切片；
//   - 多元素路径（len > 1）实现未改动，仍为 O(n) 哈希去重后 append 组装。
//
// 拷贝实现选择：标准库 slices.Clone，而不是 append([]T(nil), slice...) ——
// 后者会把"非 nil 空切片"变成 nil（JSON 序列化由 [] 变 null），
// slices.Clone 对 nil / 非 nil 空切片的 nil 语义逐位保留（其实现 s[:0:0] + append 保留 nil）。
func SliceRemoveDuplicates[T comparable](slice []T) []T {
	if len(slice) <= 1 {
		return slices.Clone(slice)
	}
	uniqueMap := make(map[T]struct{}, len(slice))
	var uniqueSlice []T

	for _, item := range slice {
		if _, exists := uniqueMap[item]; !exists {
			uniqueMap[item] = struct{}{}
			uniqueSlice = append(uniqueSlice, item)
		}
	}
	return uniqueSlice
}

// Comparator 定义比较函数类型
type Comparator[T any] func(a, b T) bool

// UniqueWithComparator 使用自定义比较函数进行去重，支持任何类型包括结构体。
//
// 与 SliceRemoveDuplicates 的契约差异（F-33，调用方须知）：
//   - len <= 1 时返回**原切片**（不拷贝），与 SliceRemoveDuplicates 的
//     "始终返回新切片"（P3-6）不一致——如需对齐另立条目，不就地改行为；
//   - 逐元素与结果集线性比对，复杂度 O(n²)；大切片去重请优先使用
//     SliceRemoveDuplicates（comparable 时，O(n)）。
func UniqueWithComparator[T any](slice []T, eq Comparator[T]) []T {
	if len(slice) <= 1 {
		return slice
	}

	result := make([]T, 0, len(slice))

	for _, item := range slice {
		isDuplicate := false
		// 检查当前元素是否与结果中的任何元素重复
		for _, existing := range result {
			if eq(item, existing) {
				isDuplicate = true
				break
			}
		}
		// 如果不是重复元素，则添加到结果中
		if !isDuplicate {
			result = append(result, item)
		}
	}

	return result
}
