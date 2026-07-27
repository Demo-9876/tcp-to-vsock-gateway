package protocol

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrame(&b, REQHead, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}
	typ, payload, err := ReadFrame(&b, 1024)
	if err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	if typ != REQHead || string(payload) != `{"ok":true}` {
		t.Fatalf("frame = 0x%02x %q", typ, payload)
	}
}

func TestReadHeaderRejectsOversizedFrame(t *testing.T) {
	raw := []byte{REQBody, 0, 0, 0, 10}
	_, err := ReadHeader(bytes.NewReader(raw), 9)
	if err == nil {
		t.Fatal("ReadHeader() expected error")
	}
}
