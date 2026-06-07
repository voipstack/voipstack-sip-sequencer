package recorder

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Given an RTP packet; When encoded with rtpdumpRecord then decoded;
// Then the recovered bytes are byte-for-byte identical (AC2: no transcoding).
func TestRtpdumpRecordPreservesPacketBytes(t *testing.T) {
	pkt := []byte{0x80, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0xFF, 0xDE, 0xAD, 0xBE, 0xEF}
	offsetMs := uint32(150)

	rec := rtpdumpRecord(pkt, offsetMs)

	if len(rec) != 8+len(pkt) {
		t.Fatalf("record length: want %d, got %d", 8+len(pkt), len(rec))
	}
	length := binary.BigEndian.Uint16(rec[0:2])
	plen := binary.BigEndian.Uint16(rec[2:4])
	gotOffset := binary.BigEndian.Uint32(rec[4:8])

	if int(length) != 8+len(pkt) {
		t.Errorf("length field: want %d, got %d", 8+len(pkt), length)
	}
	if int(plen) != len(pkt) {
		t.Errorf("plen field: want %d, got %d", len(pkt), plen)
	}
	if gotOffset != offsetMs {
		t.Errorf("offset field: want %d, got %d", offsetMs, gotOffset)
	}
	if !bytes.Equal(rec[8:], pkt) {
		t.Errorf("packet payload mismatch: want %x, got %x", pkt, rec[8:])
	}
}

// Given a file header is written then packet records appended;
// When decodeRtpdump reads the result; Then all packets are recovered intact.
func TestRtpdumpRoundTrip(t *testing.T) {
	pkts := [][]byte{
		{0x80, 0x00, 0x00, 0x01, 0xAA, 0xBB},
		{0x80, 0x00, 0x00, 0x02, 0xCC, 0xDD, 0xEE},
	}

	var buf bytes.Buffer
	buf.Write(rtpdumpFileHeader("127.0.0.1", 5004, 1000000, 0))
	for i, p := range pkts {
		buf.Write(rtpdumpRecord(p, uint32(i*20)))
	}

	got, err := decodeRtpdump(buf.Bytes())
	if err != nil {
		t.Fatalf("decodeRtpdump: %v", err)
	}
	if len(got) != len(pkts) {
		t.Fatalf("want %d packets, got %d", len(pkts), len(got))
	}
	for i, want := range pkts {
		if !bytes.Equal(got[i], want) {
			t.Errorf("pkt[%d]: want %x, got %x", i, want, got[i])
		}
	}
}

// decodeRtpdump reads an rtpdump byte slice and returns the raw packet payloads.
func decodeRtpdump(data []byte) ([][]byte, error) {
	// skip text header line
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return nil, bytes.ErrTooLarge
	}
	data = data[nl+1:]
	if len(data) < 16 {
		return nil, bytes.ErrTooLarge
	}
	data = data[16:] // skip 16-byte binary header

	var out [][]byte
	for len(data) >= 8 {
		length := int(binary.BigEndian.Uint16(data[0:2]))
		plen := int(binary.BigEndian.Uint16(data[2:4]))
		if plen > len(data)-8 || length < 8 {
			break
		}
		pkt := make([]byte, plen)
		copy(pkt, data[8:8+plen])
		out = append(out, pkt)
		data = data[length:]
	}
	return out, nil
}
