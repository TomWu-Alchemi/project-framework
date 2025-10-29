package util

func SliceRemoveDuplicates[T comparable](slice []T) []T {
	if len(slice) <= 1 {
		return slice
	}
	uniqueMap := make(map[T]struct{})
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

// UniqueWithComparator 使用自定义比较函数进行去重，支持任何类型包括结构体
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
