package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type Master struct {
	clients          sync.Map
	config           Config
	storage          *Storage
	listener         net.Listener
	closeOnce        sync.Once
	stopCh           chan struct{}
	pendingGets      sync.Map
	replicationDrops atomic.Uint64
	timestampMu      sync.Mutex
	lastTimestamp    int64
}

func NewMaster(storages ...*Storage) *Master {
	return NewMasterWithConfig(Config{}, storages...)
}

func NewMasterWithConfig(config Config, storages ...*Storage) *Master {
	storage := NewStorage()
	if len(storages) > 0 && storages[0] != nil {
		storage = storages[0]
	}
	m := &Master{
		config:  config,
		storage: storage,
		stopCh:  make(chan struct{}),
	}

	go storage.janitor(storageJanitorPeriod, storageJanitorShards, storageJanitorBatch, m.stopCh)
	return m
}

func (m *Master) Start(address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	m.listener = listener

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				log.Printf("accept peer connection: %v", err)
				continue
			}
			go m.handlePeer(conn)
		}
	}()
	return nil
}

func (m *Master) Stop() {
	m.closeOnce.Do(func() {
		close(m.stopCh)
		if m.listener != nil {
			_ = m.listener.Close()
		}
		m.clients.Range(func(_, value any) bool {
			if client, ok := value.(*Client); ok {
				client.Close()
			}
			return true
		})
	})
}

func (m *Master) Connect(address string) {
	go func() {
		for {
			select {
			case <-m.stopCh:
				return
			default:
			}
			client, err := DialToMaster(address, m.config.Password)
			if err != nil {
				log.Printf("connect peer %s: %v", address, err)
				if !m.waitToRetry(peerConnectRetry) {
					return
				}
				continue
			}
			key := "out:" + address + ":" + client.conn.LocalAddr().String()
			client.startReplicationWorker()
			m.clients.Store(key, client)
			m.sendSnapshot(client)
			m.readPeer(client, key)
			if !m.waitToRetry(peerReconnectDelay) {
				return
			}
		}
	}()
}

func (m *Master) waitToRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-m.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

func (m *Master) handlePeer(conn net.Conn) {
	client, err := authenticateIncoming(conn, m.config.Password)
	if err != nil {
		_ = conn.Close()
		log.Println(err)
		return
	}
	key := "in:" + conn.RemoteAddr().String()
	client.startReplicationWorker()
	m.clients.Store(key, client)
	m.sendSnapshot(client)
	m.readPeer(client, key)
}

func (m *Master) readPeer(client *Client, key string) {
	defer func() {
		client.Close()
		m.clients.Delete(key)
	}()
	for {
		cmd, payload, err := client.ReadPacket()
		if err != nil {
			return
		}
		switch cmd {
		case N_SET:
			operation, err := decodeOperation(payload)
			if err != nil {
				log.Printf("invalid operation from peer: %v", err)
				continue
			}
			m.applyOwnedOperation(operation)
		case N_DEL:
			k, timestamp, err := decodeDelete(payload)
			if err != nil {
				log.Printf("invalid delete from peer: %v", err)
				continue
			}
			m.applyOperation(wireOperation{Key: k, Timestamp: timestamp}, true)
		case N_GET:
			id, k, err := decodePeerGetRequest(payload)
			if err != nil {
				_ = client.Send(A_ERR, []byte("invalid get request"))
				continue
			}
			item, found := m.storage.GetRecord(k)
			response := peerGetResponse{ID: id, Found: found}
			if found {
				response.Value = item.Value
				response.ExpiresAt = item.ExpiresAt
				response.Timestamp = item.Timestamp
			}
			encoded, err := encodePeerGetResponse(response)
			if err != nil {
				continue
			}
			if err := client.Send(A_ROW, encoded); err != nil {
				return
			}
		case N_SYNC:
			m.sendSnapshot(client)
		case N_PING:
			if err := client.Send(N_PONG, nil); err != nil {
				return
			}
		case N_PONG, A_OK:
			continue
		case A_ROW:
			response, err := decodePeerGetResponse(payload)
			if err != nil {
				continue
			}
			if pending, ok := m.pendingGets.Load(response.ID); ok {
				pending.(chan peerGetResponse) <- response
			}
		case A_EXIT:
			return
		default:
			_ = client.Send(A_ERR, []byte("unsupported command"))
		}
	}
}

// Get — только uint64-ключ (используется репликацией и внутри).
func (m *Master) Get(key uint64) ([]byte, bool) {
	if key == 0 {
		return nil, false
	}
	return m.storage.Get(key)
}

// Set — uint64-ключ (используется репликацией).
func (m *Master) Set(key uint64, value []byte, ttl int64) error {
	return m.set(key, value, ttl)
}

// setOwned — uint64-ключ, вызывается из handleLocal после хэширования.
func (m *Master) setOwned(key uint64, value []byte, ttl int64) error {
	return m.set(key, value, ttl)
}

func (m *Master) set(key uint64, value []byte, ttl int64) error {
	if key == 0 || ttl < 0 {
		return errors.New("invalid key or TTL")
	}
	compressed, err := compressValue(value)
	if err != nil {
		return fmt.Errorf("compress value: %w", err)
	}
	var expiresAt int64
	if ttl > 0 {
		expiresAt = time.Now().Add(time.Duration(ttl) * time.Second).UnixNano()
	}
	operation := wireOperation{Key: key, Value: compressed, ExpiresAt: expiresAt, Timestamp: m.nextTimestamp()}
	m.applyOwnedOperation(operation)
	m.broadcastOperation(operation, false)
	return nil
}

// Delete — uint64-ключ.
func (m *Master) Delete(key uint64) error {
	if key == 0 {
		return errors.New("empty key")
	}
	operation := wireOperation{Key: key, Timestamp: m.nextTimestamp()}
	m.applyOperation(operation, true)
	m.broadcastOperation(operation, true)
	return nil
}

// ReplicationDrops — сколько доставок пропущено из-за переполнения очереди.
func (m *Master) ReplicationDrops() uint64 {
	return m.replicationDrops.Load()
}

func (m *Master) nextTimestamp() int64 {
	m.timestampMu.Lock()
	defer m.timestampMu.Unlock()
	now := time.Now().UnixNano()
	if now <= m.lastTimestamp {
		now = m.lastTimestamp + 1
	}
	m.lastTimestamp = now
	return now
}

func (m *Master) applyOperation(operation wireOperation, deleted bool) {
	m.timestampMu.Lock()
	if operation.Timestamp > m.lastTimestamp {
		m.lastTimestamp = operation.Timestamp
	}
	m.timestampMu.Unlock()
	if deleted {
		m.storage.ApplyDelete(operation.Key, operation.Timestamp)
		return
	}
	m.storage.ApplySet(operation.Key, operation.Value, operation.ExpiresAt, operation.Timestamp)
}

func (m *Master) applyOwnedOperation(operation wireOperation) {
	m.timestampMu.Lock()
	if operation.Timestamp > m.lastTimestamp {
		m.lastTimestamp = operation.Timestamp
	}
	m.timestampMu.Unlock()
	m.storage.applySetOwned(operation.Key, operation.Value, operation.ExpiresAt, operation.Timestamp)
}

func (m *Master) broadcastOperation(operation wireOperation, deleted bool) {
	var payload []byte
	var encodeErr error
	encoded := false
	cmd := N_SET
	if deleted {
		cmd = N_DEL
	}
	m.clients.Range(func(key, value any) bool {
		client, ok := value.(*Client)
		if !ok {
			return true
		}
		if !encoded {
			if deleted {
				payload, encodeErr = encodeDelete(operation.Key, operation.Timestamp)
			} else {
				payload, encodeErr = encodeOperation(operation)
			}
			if encodeErr != nil {
				return false
			}
			encoded = true
		}
		if !client.enqueueReplication(cmd, payload) {
			m.replicationDrops.Add(1)
		}
		return true
	})
	if encodeErr != nil {
		log.Printf("encode replication operation: %v", encodeErr)
	}
}

func (m *Master) sendSnapshot(client *Client) {
	snapshot := m.storage.Snapshot()
	keys := make([]uint64, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	batch := make([]outboundPacket, 0, replicationBatchSize)
	batchBytes := 0
	for _, key := range keys {
		item := snapshot[key]
		operation := wireOperation{Key: key, Value: item.Value, ExpiresAt: item.ExpiresAt, Timestamp: item.Timestamp}
		var payload []byte
		var err error
		if item.Deleted {
			payload, err = encodeDelete(key, item.Timestamp)
		} else {
			payload, err = encodeOperation(operation)
		}
		if err != nil {
			continue
		}
		cmd := N_SET
		if item.Deleted {
			cmd = N_DEL
		}
		batch = append(batch, outboundPacket{command: cmd, payload: payload})
		batchBytes += len(payload) + HEADER_SIZE
		if len(batch) < replicationBatchSize && batchBytes < replicationBatchMaxBytes {
			continue
		}
		if err := client.sendBatch(batch); err != nil {
			client.Close()
			return
		}
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
	}
	if len(batch) > 0 {
		if err := client.sendBatch(batch); err != nil {
			client.Close()
		}
		clear(batch)
	}
}

// handleLocal — единственное место, где строковые ключи от PHP
// превращаются в uint64 через fnv1a64.
func (m *Master) handleLocal(client *Client) {
	defer client.Close()
	for {
		cmd, payload, err := client.ReadPacket()
		if err != nil {
			return
		}
		switch cmd {
		case L_GET:
			key := fnv1a64Bytes(payload)
			value, found := m.Get(key)
			if !found {
				err = client.Send(A_EMP, nil)
			} else {
				err = client.Send(A_ROW, value)
			}

		case L_SET:
			var request localSetRequest
			if request, err = decodeLocalSet(payload); err == nil {
				keyHash := fnv1a64Bytes(request.Key)
				err = m.setOwned(keyHash, request.Value, request.TTL)
			}
			if err == nil {
				err = client.Send(A_OK, nil)
			}

		case L_DEL:
			key := fnv1a64(string(payload))
			err = m.Delete(key)
			if err == nil {
				err = client.Send(A_OK, nil)
			}

		case N_PING:
			err = client.Send(N_PONG, nil)

		default:
			err = errors.New("unsupported command")
		}
		if err != nil {
			if sendErr := client.Send(A_ERR, []byte(err.Error())); sendErr != nil {
				return
			}
		}
	}
}
