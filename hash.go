package main

// fnv1a64 — быстрый не-криптографический хэш строки в uint64.
// FNV-1a: простой, детерминированный, хорошо распределяет по шардам.
func fnv1a64(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	// Гарантируем ненулевой хэш (0 зарезервирован как «invalid key»)
	if h == 0 {
		h = 1
	}
	return h
}

func fnv1a64Bytes(b []byte) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(b); i++ {
		h ^= uint64(b[i])
		h *= prime64
	}
	if h == 0 {
		h = 1
	}
	return h
}
