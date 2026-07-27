package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	REQHead     byte = 0x01
	REQBody     byte = 0x02
	RespHead    byte = 0x10
	RespChunk   byte = 0x11
	RespTrailer byte = 0x12
	Err         byte = 0x1f

	HeaderSize = 5
)

type Header struct {
	Type   byte
	Length uint32
}

func ReadHeader(r io.Reader, maxPayload uint32) (Header, error) {
	var raw [HeaderSize]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return Header{}, err
	}
	h := Header{
		Type:   raw[0],
		Length: binary.BigEndian.Uint32(raw[1:]),
	}
	if h.Length > maxPayload {
		return Header{}, fmt.Errorf("frame length %d exceeds limit %d", h.Length, maxPayload)
	}
	return h, nil
}

func ReadPayload(r io.Reader, n uint32) ([]byte, error) {
	payload := make([]byte, n)
	_, err := io.ReadFull(r, payload)
	return payload, err
}

func ReadFrame(r io.Reader, maxPayload uint32) (byte, []byte, error) {
	h, err := ReadHeader(r, maxPayload)
	if err != nil {
		return 0, nil, err
	}
	payload, err := ReadPayload(r, h.Length)
	if err != nil {
		return 0, nil, err
	}
	return h.Type, payload, nil
}

func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > int(^uint32(0)) {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	var raw [HeaderSize]byte
	raw[0] = typ
	binary.BigEndian.PutUint32(raw[1:], uint32(len(payload)))
	if _, err := w.Write(raw[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
