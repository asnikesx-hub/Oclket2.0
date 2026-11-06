package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
)

type Command uint32

const (
	// LOCAL COMMAND FROM CLIENT
	L_GET Command = 0x434D4401 // GET payload: raw string key
	L_SET Command = 0x434D4402 // SET payload: [TTL(8)][KEY_LEN(4)][KEY][VAL_LEN(4)][VALUE]
	L_DEL Command = 0x434D4403 // DEL payload: raw string key

	// NETWORK COMMAND [REPLICA ACTION]
	N_GET  Command = 0x434D4411 // GET payload: [REQUEST_ID(8)][KEY(8)]
	N_SET  Command = 0x434D4412 // SET payload: [KEY(8)][EXPIRES_AT(8)][TIMESTAMP(8)][VAL_LEN(4)][VALUE]
	N_DEL  Command = 0x434D4413 // DEL payload: [TIMESTAMP(8)][KEY(8)]
	N_SYNC Command = 0x434D4414 // request a full snapshot; payload: [TIMESTAMP(8)]

	N_PING Command = 0x434D4420 // KEEP-ALIVE RQ (WITH EMPTY PAYLOAD)
	N_PONG Command = 0x434D4421 // KEEP-ALIVE RS (WITH EMPTY PAYLOAD)

	// ANSWER COMMAND
	A_OK   Command = 0x434D4430 // BASIC ANSWER FOR: [ _SET | _DEL | _SYNC ] (WITH EMPTY PAYLOAD)
	A_ERR  Command = 0x434D4431 // ERROR payload: UTF-8 message
	A_ROW  Command = 0x434D4432 // local GET: raw value; peer GET: binary response
	A_EMP  Command = 0x434D4433 // EMPTY PACKET
	A_EXIT Command = 0x434D44FF // CLOSE CLIENT CONNECTION (EMPTY PAYLOAD)

	C_AUTH_CH Command = 0x434D44A1
	C_AUTH_VF Command = 0x434D44A2

	MAX_PAYLOAD_SIZE = 128 * 1024 * 1024
	MAX_CHUNK_SIZE   = 128 * 1024
	HEADER_SIZE      = 8
)

var (
	ErrHeaderRead = errors.New("failed to read header")
	ErrPeekFailed = errors.New("peek failed")
	ErrPayloadMax = errors.New("payload size exceeds max")
	ErrChankRead  = errors.New("failed to read chunk")
)

type PacketHandler func(cmd Command, payload []byte)

func ReadPacket(reader *bufio.Reader) (Command, []byte, error) {
	header, err := reader.Peek(HEADER_SIZE)
	if err != nil {
		return 0, nil, ErrHeaderRead
	}

	cmd := Command(binary.BigEndian.Uint32(header[0:4]))
	payloadLen := binary.BigEndian.Uint32(header[4:8])

	if payloadLen > MAX_PAYLOAD_SIZE {
		reader.Discard(HEADER_SIZE)
		return 0, nil, ErrPayloadMax
	}

	totalSize := HEADER_SIZE + int(payloadLen)

	if totalSize <= reader.Buffered() {
		data, err := reader.Peek(totalSize)
		if err != nil {
			return 0, nil, ErrPeekFailed
		}

		payload := make([]byte, int(payloadLen))
		copy(payload, data[HEADER_SIZE:totalSize])

		reader.Discard(totalSize)
		return cmd, payload, nil
	}

	reader.Discard(HEADER_SIZE)

	payload := make([]byte, int(payloadLen))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, ErrChankRead
	}

	return cmd, payload, nil
}

func WritePacket(writer *bufio.Writer, cmd Command, payload []byte) error {
	if err := writePacketFrame(writer, cmd, payload); err != nil {
		return err
	}
	return writer.Flush()
}

func writePacketFrame(writer *bufio.Writer, cmd Command, payload []byte) error {
	payloadLen := len(payload)
	if payloadLen > MAX_PAYLOAD_SIZE {
		return ErrPayloadMax
	}

	var header [HEADER_SIZE]byte
	binary.BigEndian.PutUint32(header[:4], uint32(cmd))
	binary.BigEndian.PutUint32(header[4:], uint32(payloadLen))

	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	if payloadLen > 0 {
		if _, err := writer.Write(payload); err != nil {
			return err
		}
	}
	return nil
}
