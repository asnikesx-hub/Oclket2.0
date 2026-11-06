package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	replicationQueueCapacity = 1024
	replicationQueueMaxBytes = 64 << 20
	replicationBatchSize     = 64
	replicationBatchMaxBytes = 1 << 20
	replicationBatchDelay    = time.Millisecond
	replicationWriterSize    = 64 << 10
)

type outboundPacket struct {
	command Command
	payload []byte
}

type Client struct {
	writeMu   sync.Mutex
	closeOnce sync.Once
	queueOnce sync.Once

	conn             net.Conn
	reader           *bufio.Reader
	writer           *bufio.Writer
	authKey          []byte
	closed           chan struct{}
	replicationQueue chan outboundPacket
	queuedBytes      atomic.Int64
}

func DialToMaster(host, password string) (*Client, error) {
	conn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}

	client := NewClient(conn)
	if err := client.Auth(password); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func NewClient(conn net.Conn) *Client {
	return &Client{
		conn:    conn,
		reader:  bufio.NewReader(conn),
		writer:  bufio.NewWriter(conn),
		authKey: generateRandomKey(peerAuthKeySize),
		closed:  make(chan struct{}),
	}
}

func (c *Client) Auth(password string) error {
	if err := c.conn.SetDeadline(time.Now().Add(peerIOTimeout)); err != nil {
		return err
	}
	defer c.conn.SetDeadline(time.Time{})

	if err := c.Send(C_AUTH_CH, c.authKey); err != nil {
		return err
	}
	cmd, payload, err := c.ReadPacket()
	if err != nil {
		return err
	}
	if cmd != C_AUTH_VF {
		return fmt.Errorf("bad response from auth")
	}
	if !bytes.Equal(signMessage(c.authKey, []byte(password)), payload) {
		return fmt.Errorf("bad signed string")
	}
	return nil
}

func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *Client) Send(cmd Command, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(peerIOTimeout)); err != nil {
		return err
	}
	defer c.conn.SetWriteDeadline(time.Time{})
	return WritePacket(c.writer, cmd, payload)
}

func (c *Client) startReplicationWorker() {
	c.queueOnce.Do(func() {
		c.writer = bufio.NewWriterSize(c.conn, replicationWriterSize)
		c.replicationQueue = make(chan outboundPacket, replicationQueueCapacity)
		go c.runReplicationWorker()
	})
}

func (c *Client) enqueueReplication(command Command, payload []byte) bool {
	if c.replicationQueue == nil {
		return false
	}
	size := int64(len(payload) + HEADER_SIZE)
	if size > replicationQueueMaxBytes {
		return false
	}
	for {
		queued := c.queuedBytes.Load()
		if queued+size > replicationQueueMaxBytes {
			return false
		}
		if c.queuedBytes.CompareAndSwap(queued, queued+size) {
			break
		}
	}
	select {
	case c.replicationQueue <- outboundPacket{command: command, payload: payload}:
		return true
	default:
		c.queuedBytes.Add(-size)
		return false
	}
}

func (c *Client) runReplicationWorker() {
	for {
		var batch []outboundPacket
		select {
		case <-c.closed:
			return
		case packet := <-c.replicationQueue:
			batch = append(batch, packet)
		}

		timer := time.NewTimer(replicationBatchDelay)
	collect:
		for len(batch) < replicationBatchSize {
			select {
			case <-c.closed:
				timer.Stop()
				return
			case packet := <-c.replicationQueue:
				batch = append(batch, packet)
			case <-timer.C:
				break collect
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		if err := c.sendBatch(batch); err != nil {
			c.Close()
			return
		}
		for _, packet := range batch {
			c.queuedBytes.Add(-int64(len(packet.payload) + HEADER_SIZE))
		}
	}
}

func (c *Client) sendBatch(packets []outboundPacket) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(peerIOTimeout)); err != nil {
		return err
	}
	defer c.conn.SetWriteDeadline(time.Time{})
	for _, packet := range packets {
		if err := writePacketFrame(c.writer, packet.command, packet.payload); err != nil {
			return err
		}
	}
	return c.writer.Flush()
}

func (c *Client) ReadPacket() (Command, []byte, error) {
	return ReadPacket(c.reader)
}

func authenticateIncoming(conn net.Conn, password string) (*Client, error) {
	client := NewClient(conn)
	if err := conn.SetDeadline(time.Now().Add(peerIOTimeout)); err != nil {
		return nil, err
	}
	cmd, key, err := client.ReadPacket()
	if err != nil {
		return nil, err
	}
	if cmd != C_AUTH_CH || len(key) == 0 {
		return nil, fmt.Errorf("invalid authentication challenge")
	}
	if err := client.Send(C_AUTH_VF, signMessage(key, []byte(password))); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return client, nil
}
