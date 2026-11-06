package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"
)

func TestReadPacketPayloadSizes(t *testing.T) {
	for _, size := range []int{0, 512, 128 * 1024} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), size)
			var frame bytes.Buffer
			writer := bufio.NewWriter(&frame)
			if err := writePacketFrame(writer, N_SET, payload); err != nil {
				t.Fatal(err)
			}
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}

			reader := bufio.NewReaderSize(bytes.NewReader(frame.Bytes()), 4096)
			cmd, got, err := ReadPacket(reader)
			if err != nil {
				t.Fatal(err)
			}
			if cmd != N_SET || !bytes.Equal(got, payload) {
				t.Fatalf("ReadPacket() returned command %v and payload length %d", cmd, len(got))
			}
		})
	}
}

func BenchmarkReadPacketSmall(b *testing.B) {
	benchmarkReadPacket(b, 512)
}

func BenchmarkReadPacketLarge(b *testing.B) {
	benchmarkReadPacket(b, 128*1024)
}

func benchmarkReadPacket(b *testing.B, payloadSize int) {
	payload := bytes.Repeat([]byte("x"), payloadSize)
	var frame bytes.Buffer
	writer := bufio.NewWriter(&frame)
	if err := writePacketFrame(writer, N_SET, payload); err != nil {
		b.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		b.Fatal(err)
	}
	data := frame.Bytes()
	readerSize := len(data)
	if payloadSize > 4096 {
		readerSize = 4096
	}
	reader := bufio.NewReaderSize(bytes.NewReader(data), readerSize)

	b.ReportAllocs()
	b.SetBytes(int64(payloadSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := ReadPacket(reader); err != nil {
			b.Fatal(err)
		}
		reader.Reset(bytes.NewReader(data))
	}
}

func BenchmarkSendSnapshot(b *testing.B) {
	storage := NewStorage()
	value := bytes.Repeat([]byte("snapshot-value-"), 32)
	for i := uint64(1); i <= 1024; i++ {
		storage.applySetOwned(i, value, 0, int64(i))
	}
	master := &Master{storage: storage}

	server, peer := net.Pipe()
	client := NewClient(server)
	client.startReplicationWorker()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, peer)
		close(done)
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		master.sendSnapshot(client)
	}
	b.StopTimer()
	client.Close()
	_ = peer.Close()
	<-done
}
