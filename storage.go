package main

import (
	"sync"
	"time"
)

const STORAGE_SHARD = 64

type StorageItem struct {
	Value     []byte
	ExpiresAt int64
	Timestamp int64
	Deleted   bool
}

type Shard struct {
	mu   sync.RWMutex
	data map[uint64]*StorageItem
}

type Storage struct {
	shards [STORAGE_SHARD]*Shard
}

func NewStorage() *Storage {
	s := &Storage{}
	for i := range s.shards {
		s.shards[i] = &Shard{
			data: make(map[uint64]*StorageItem, 1024),
		}
	}
	return s
}

func (s *Storage) shardIndex(key uint64) uint32 {
	return uint32(key % STORAGE_SHARD)
}

func (s *Storage) Set(key uint64, value []byte, ttl int64, tm int64) bool {
	var expiresAt int64
	if ttl > 0 {
		expiresAt = time.Now().Add(time.Duration(ttl) * time.Second).UnixNano()
	}
	return s.ApplySet(key, value, expiresAt, tm)
}

func (s *Storage) ApplySet(key uint64, value []byte, expiresAt int64, tm int64) bool {
	return s.applySet(key, value, expiresAt, tm, true)
}

// applySetOwned stores value without copying; the caller must not mutate it afterward.
func (s *Storage) applySetOwned(key uint64, value []byte, expiresAt int64, tm int64) bool {
	return s.applySet(key, value, expiresAt, tm, false)
}

func (s *Storage) applySet(key uint64, value []byte, expiresAt int64, tm int64, copyValue bool) bool {
	idx := s.shardIndex(key)
	shard := s.shards[idx]

	shard.mu.Lock()
	defer shard.mu.Unlock()
	if current, exists := shard.data[key]; exists && tm < current.Timestamp {
		return false
	}
	if copyValue {
		value = append([]byte(nil), value...)
	}

	shard.data[key] = &StorageItem{
		Value:     value,
		ExpiresAt: expiresAt,
		Timestamp: tm,
	}
	return true
}

// Get возвращает РАЗЖАТОЕ значение.
func (s *Storage) Get(key uint64) ([]byte, bool) {
	compressed, found := s.getView(key)
	if !found {
		return nil, false
	}
	value, err := decompressValue(compressed)
	if err != nil {
		// не LZ4 — вернуть как есть
		return compressed, true
	}
	return value, true
}

// getView возвращает СЖАТОЕ значение (для репликации, snapshot и peer GET).
func (s *Storage) getView(key uint64) ([]byte, bool) {
	if key == 0 {
		return nil, false
	}

	shard := s.shards[s.shardIndex(key)]

	shard.mu.RLock()
	item, exists := shard.data[key]

	if !exists || item.Deleted {
		shard.mu.RUnlock()
		return nil, false
	}

	if item.ExpiresAt != 0 && time.Now().UnixNano() >= item.ExpiresAt {
		shard.mu.RUnlock()
		return nil, false
	}
	value := item.Value
	shard.mu.RUnlock()
	return value, true
}

// GetRecord возвращает СЖАТОЕ значение внутри StorageItem.
func (s *Storage) GetRecord(key uint64) (StorageItem, bool) {
	idx := s.shardIndex(key)
	shard := s.shards[idx]

	shard.mu.RLock()
	item, exists := shard.data[key]
	if !exists {
		shard.mu.RUnlock()
		return StorageItem{}, false
	}

	if item.Deleted || (item.ExpiresAt != 0 && time.Now().UnixNano() >= item.ExpiresAt) {
		shard.mu.RUnlock()
		return StorageItem{}, false
	}

	copy := item
	copy.Value = append([]byte(nil), item.Value...)
	shard.mu.RUnlock()
	return *copy, true
}

func (s *Storage) Delete(key uint64) int {
	idx := s.shardIndex(key)
	shard := s.shards[idx]

	shard.mu.Lock()
	defer shard.mu.Unlock()

	current, exists := shard.data[key]
	if !exists || current.Deleted {
		return 0
	}
	timestamp := time.Now().UnixNano()
	if timestamp <= current.Timestamp {
		timestamp = current.Timestamp + 1
	}
	shard.data[key] = &StorageItem{Timestamp: timestamp, Deleted: true}
	return 1
}

func (s *Storage) ApplyDelete(key uint64, tm int64) bool {
	idx := s.shardIndex(key)
	shard := s.shards[idx]

	shard.mu.Lock()
	defer shard.mu.Unlock()
	if current, exists := shard.data[key]; exists && tm < current.Timestamp {
		return false
	}
	shard.data[key] = &StorageItem{Timestamp: tm, Deleted: true}
	return true
}

func (s *Storage) Snapshot() map[uint64]StorageItem {
	result := make(map[uint64]StorageItem)
	for _, shard := range s.shards {
		shard.mu.RLock()
		for key, item := range shard.data {
			copy := item
			copy.Value = append([]byte(nil), item.Value...)
			result[key] = *copy
		}
		shard.mu.RUnlock()
	}
	return result
}

func (s *Storage) sweepShard(shard *Shard, now int64, batchSize int) {
	for {
		var expired []uint64

		shard.mu.RLock()
		for key, item := range shard.data {
			if item.ExpiresAt != 0 && now >= item.ExpiresAt {
				expired = append(expired, key)
				if len(expired) >= batchSize {
					break
				}
			}
		}
		shard.mu.RUnlock()

		if len(expired) == 0 {
			return
		}

		shard.mu.Lock()
		for _, key := range expired {
			if item, ok := shard.data[key]; ok &&
				item.ExpiresAt != 0 && now >= item.ExpiresAt {
				delete(shard.data, key)
			}
		}
		shard.mu.Unlock()

		if len(expired) < batchSize {
			return
		}
	}
}

func (s *Storage) janitor(interval time.Duration, shardsPerTick int, batchSize int, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var next int
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			now := time.Now().UnixNano()
			for i := 0; i < shardsPerTick; i++ {
				shard := s.shards[next]
				next = (next + 1) % STORAGE_SHARD
				s.sweepShard(shard, now, batchSize)
			}
		}
	}
}
