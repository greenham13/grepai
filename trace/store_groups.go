package trace

// groupFileEntries avoids slice growth and repeated per-entry map updates.
// Single-file lists transfer ownership without a copy. Mixed lists get
// independent backing arrays so deleting a file releases its entries.
func groupFileEntries[T any](list []T, fileOf func(T) string) map[string][]T {
	counts := make(map[string]int)
	for i := 0; i < len(list); {
		file := fileOf(list[i])
		j := i + 1
		for j < len(list) && fileOf(list[j]) == file {
			j++
		}
		counts[file] += j - i
		i = j
	}
	out := make(map[string][]T, len(counts))
	if len(counts) == 1 {
		for file := range counts {
			out[file] = list[:len(list):len(list)]
		}
		return out
	}
	for file, n := range counts {
		out[file] = make([]T, 0, n)
	}
	for i := 0; i < len(list); {
		file := fileOf(list[i])
		j := i + 1
		for j < len(list) && fileOf(list[j]) == file {
			j++
		}
		out[file] = append(out[file], list[i:j]...)
		i = j
	}
	return out
}
