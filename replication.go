package main

import (
	"encoding/binary"
	"fmt"
)

// wireOperation — сетевая операция репликации. Ключ — uint64.
type wireOperation struct {
	Key       uint64
	Value     []byte
	ExpiresAt int64
	Timestamp int64
}

// localSetRequest — локальный SET от PHP. Ключ — строка.
// Хэширование в uint64 происходит в Master.handleLocal.
type localSetRequest struct {
	Key   []byte
	Value []byte
	TTL   int64
}

// peerGetResponse — ответ на peer GET. Ключ uint64.
type peerGetResponse struct {
	ID        uint64
	Found     bool
	Value     []byte
	ExpiresAt int64
	Timestamp int64
}

// LOCAL SET (L_SET) — строковый ключ
// Формат: [TTL(8)][KEY_LEN(4)][KEY][VAL_LEN(4)][VALUE]

func encodeLocalSet(request localSetRequest) ([]byte, error) {
	if request.TTL < 0 ||
		len(request.Key) == 0 ||
		uint64(len(request.Key)) > uint64(^uint32(0)) ||
		uint64(len(request.Value)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("invalid SET request")
	}
	payload := make([]byte, 16+len(request.Key)+len(request.Value))
	putInt64(payload[:8], request.TTL)
	putUint32(payload[8:12], uint32(len(request.Key)))
	copy(payload[12:], request.Key)
	offset := 12 + len(request.Key)
	putUint32(payload[offset:offset+4], uint32(len(request.Value)))
	copy(payload[offset+4:], request.Value)
	return payload, nil
}

func decodeLocalSet(payload []byte) (localSetRequest, error) {
	if len(payload) < 16 {
		return localSetRequest{}, fmt.Errorf("invalid SET payload")
	}
	ttl := readInt64(payload[:8])
	keyLen := int(readUint32(payload[8:12]))
	if ttl < 0 || keyLen == 0 || keyLen > len(payload)-16 {
		return localSetRequest{}, fmt.Errorf("invalid SET payload")
	}
	offset := 12 + keyLen
	valueLen := int(readUint32(payload[offset : offset+4]))
	if valueLen != len(payload)-offset-4 {
		return localSetRequest{}, fmt.Errorf("invalid SET value length")
	}
	return localSetRequest{
		Key:   payload[12:offset],
		Value: payload[offset+4:],
		TTL:   ttl,
	}, nil
}

// NETWORK SET (N_SET) — uint64-ключ
// Формат: [KEY(8)][EXPIRES_AT(8)][TIMESTAMP(8)][VAL_LEN(4)][VALUE]

func encodeOperation(operation wireOperation) ([]byte, error) {
	if operation.Key == 0 ||
		operation.Timestamp <= 0 ||
		uint64(len(operation.Value)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("invalid replication operation")
	}
	payload := make([]byte, 28+len(operation.Value))
	putUint64(payload[:8], operation.Key)
	putInt64(payload[8:16], operation.ExpiresAt)
	putInt64(payload[16:24], operation.Timestamp)
	putUint32(payload[24:28], uint32(len(operation.Value)))
	copy(payload[28:], operation.Value)
	return payload, nil
}

func decodeOperation(payload []byte) (wireOperation, error) {
	if len(payload) < 28 {
		return wireOperation{}, fmt.Errorf("invalid replication payload")
	}
	key := readUint64(payload[:8])
	valueLen := int(readUint32(payload[24:28]))
	if key == 0 || valueLen != len(payload)-28 {
		return wireOperation{}, fmt.Errorf("invalid replication value length")
	}
	operation := wireOperation{
		Key:       key,
		ExpiresAt: readInt64(payload[8:16]),
		Timestamp: readInt64(payload[16:24]),
		Value:     payload[28:],
	}
	if operation.Timestamp <= 0 {
		return wireOperation{}, fmt.Errorf("invalid replication timestamp")
	}
	return operation, nil
}

// NETWORK DEL (N_DEL) — uint64-ключ
// Формат: [TIMESTAMP(8)][KEY(8)]

func encodeDelete(key uint64, timestamp int64) ([]byte, error) {
	if key == 0 || timestamp <= 0 {
		return nil, fmt.Errorf("invalid replication delete")
	}
	payload := make([]byte, 16)
	putInt64(payload[:8], timestamp)
	putUint64(payload[8:16], key)
	return payload, nil
}

func decodeDelete(payload []byte) (uint64, int64, error) {
	if len(payload) != 16 {
		return 0, 0, fmt.Errorf("invalid replication delete payload")
	}
	timestamp := readInt64(payload[:8])
	key := readUint64(payload[8:16])
	if timestamp <= 0 || key == 0 {
		return 0, 0, fmt.Errorf("invalid replication delete timestamp or key")
	}
	return key, timestamp, nil
}

// NETWORK GET (N_GET) — uint64-ключ
// Формат: [REQUEST_ID(8)][KEY(8)]

func encodePeerGetRequest(id uint64, key uint64) []byte {
	payload := make([]byte, 16)
	putUint64(payload[:8], id)
	putUint64(payload[8:16], key)
	return payload
}

func decodePeerGetRequest(payload []byte) (uint64, uint64, error) {
	if len(payload) != 16 {
		return 0, 0, fmt.Errorf("invalid peer GET payload")
	}
	id := readUint64(payload[:8])
	key := readUint64(payload[8:16])
	if id == 0 || key == 0 {
		return 0, 0, fmt.Errorf("invalid peer GET request ID or key")
	}
	return id, key, nil
}

// PEER GET RESPONSE (A_ROW)
// Формат: [ID(8)][FOUND(1)][EXPIRES_AT(8)][TIMESTAMP(8)][VAL_LEN(4)][VALUE]

func encodePeerGetResponse(response peerGetResponse) ([]byte, error) {
	if response.ID == 0 || uint64(len(response.Value)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("invalid peer GET response")
	}
	payload := make([]byte, 29+len(response.Value))
	putUint64(payload[:8], response.ID)
	if response.Found {
		payload[8] = 1
	}
	putInt64(payload[9:17], response.ExpiresAt)
	putInt64(payload[17:25], response.Timestamp)
	putUint32(payload[25:29], uint32(len(response.Value)))
	copy(payload[29:], response.Value)
	return payload, nil
}

func decodePeerGetResponse(payload []byte) (peerGetResponse, error) {
	if len(payload) < 29 {
		return peerGetResponse{}, fmt.Errorf("invalid peer GET response")
	}
	valueLen := int(readUint32(payload[25:29]))
	if valueLen != len(payload)-29 || payload[8] > 1 {
		return peerGetResponse{}, fmt.Errorf("invalid peer GET response fields")
	}
	response := peerGetResponse{
		ID:        readUint64(payload[:8]),
		Found:     payload[8] == 1,
		ExpiresAt: readInt64(payload[9:17]),
		Timestamp: readInt64(payload[17:25]),
		Value:     payload[29:],
	}
	if response.ID == 0 {
		return peerGetResponse{}, fmt.Errorf("invalid peer GET response ID")
	}
	return response, nil
}

// Утилиты BigEndian

func putUint32(dst []byte, value uint32) {
	binary.BigEndian.PutUint32(dst, value)
}

func readUint32(src []byte) uint32 {
	return binary.BigEndian.Uint32(src)
}

func putUint64(dst []byte, value uint64) {
	binary.BigEndian.PutUint64(dst, value)
}

func readUint64(src []byte) uint64 {
	return binary.BigEndian.Uint64(src)
}

func putInt64(dst []byte, value int64) {
	putUint64(dst, uint64(value))
}

func readInt64(src []byte) int64 {
	return int64(readUint64(src))
}
