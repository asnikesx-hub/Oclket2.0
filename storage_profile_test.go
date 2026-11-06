package main

import (
	"crypto/rand"
	"runtime"
	"sync"
	"testing"
	"time"
)

const (
	profileKeys   = 100_000
	profileValue  = 512
	profileReads  = 5_000_000
	profileWrites = 1_000_000
)

func makeKeys(n int) []uint64 {
	keys := make([]uint64, n)
	for i := range keys {
		keys[i] = uint64(i+1) * 0x9E3779B97F4A7C15
	}
	return keys
}

func fillStorage(tb testing.TB, s *Storage, keys []uint64, value []byte, ttl int64) {
	tb.Helper()
	for i, key := range keys {
		if !s.applySetOwned(key, value, ttl, int64(i+1)) {
			tb.Fatalf("Set failed for key %d", key)
		}
	}
}

func BenchmarkProfileSet(b *testing.B) {
	s := NewStorage()
	value := make([]byte, profileValue)
	if _, err := rand.Read(value); err != nil {
		b.Fatal(err)
	}
	keys := makeKeys(1024)
	b.ReportAllocs()
	b.SetBytes(int64(profileValue))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Set(keys[i&1023], value, 0, int64(i+1))
	}
}

func BenchmarkProfileGet(b *testing.B) {
	s := NewStorage()
	value := make([]byte, profileValue)
	if _, err := rand.Read(value); err != nil {
		b.Fatal(err)
	}
	keys := makeKeys(1024)
	fillStorage(b, s, keys, value, 0)
	b.ReportAllocs()
	b.SetBytes(int64(profileValue))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := keys[i&1023]
		v, found := s.Get(key)
		if !found || len(v) != profileValue {
			b.Fatalf("GET failed for key %d", key)
		}
	}
}

func BenchmarkProfileMixed(b *testing.B) {
	s := NewStorage()
	value := make([]byte, profileValue)
	if _, err := rand.Read(value); err != nil {
		b.Fatal(err)
	}
	keys := makeKeys(1024)
	fillStorage(b, s, keys, value, 0)
	b.ReportAllocs()
	b.SetBytes(int64(profileValue))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := keys[i&1023]
		if i%11 == 0 {
			s.Set(key, value, 0, int64(i+1))
		} else {
			s.Get(key)
		}
	}
}

func BenchmarkProfileGetParallel(b *testing.B) {
	s := NewStorage()
	value := make([]byte, profileValue)
	if _, err := rand.Read(value); err != nil {
		b.Fatal(err)
	}
	keys := makeKeys(4096)
	fillStorage(b, s, keys, value, 0)
	b.ReportAllocs()
	b.SetBytes(int64(profileValue))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Get(keys[i&4095])
			i++
		}
	})
}

func BenchmarkProfileSetParallel(b *testing.B) {
	s := NewStorage()
	value := make([]byte, profileValue)
	if _, err := rand.Read(value); err != nil {
		b.Fatal(err)
	}
	keys := makeKeys(4096)
	b.ReportAllocs()
	b.SetBytes(int64(profileValue))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Set(keys[i&4095], value, 0, int64(i+1))
			i++
		}
	})
}

func TestProfileLongRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping long profile test")
	}
	s := NewStorage()
	value := make([]byte, profileValue)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	keys := makeKeys(profileKeys)

	t.Logf("Phase 1: filling %d keys...", profileKeys)
	start := time.Now()
	fillStorage(t, s, keys, value, 0)
	t.Logf("  fill: %v (%.0f ops/s)", time.Since(start),
		float64(profileKeys)/time.Since(start).Seconds())

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("  heap after fill: Alloc=%d MB, Sys=%d MB",
		m.Alloc/1024/1024, m.Sys/1024/1024)

	t.Logf("Phase 2: %d reads...", profileReads)
	start = time.Now()
	for i := 0; i < profileReads; i++ {
		key := keys[i%profileKeys]
		v, found := s.Get(key)
		if !found || v == nil {
			t.Fatalf("GET failed at %d (key=%d)", i, key)
		}
	}
	t.Logf("  read: %v (%.0f ops/s)", time.Since(start),
		float64(profileReads)/time.Since(start).Seconds())

	t.Logf("Phase 3: %d writes...", profileWrites)
	start = time.Now()
	for i := 0; i < profileWrites; i++ {
		s.Set(keys[i%profileKeys], value, 0, int64(profileKeys+i+1))
	}
	t.Logf("  write: %v (%.0f ops/s)", time.Since(start),
		float64(profileWrites)/time.Since(start).Seconds())

	t.Logf("Phase 4: parallel reads...")
	start = time.Now()
	var wg sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	perWorker := profileReads / workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				s.Get(keys[(offset+i)%profileKeys])
			}
		}(w * perWorker)
	}
	wg.Wait()
	total := workers * perWorker
	t.Logf("  parallel read: %v (%.0f ops/s)", time.Since(start),
		float64(total)/time.Since(start).Seconds())

	runtime.ReadMemStats(&m)
	t.Logf("Heap: Alloc=%d MB, Sys=%d MB, NumGC=%d",
		m.Alloc/1024/1024, m.Sys/1024/1024, m.NumGC)
}
