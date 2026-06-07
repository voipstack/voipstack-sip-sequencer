package recorder

import (
	"encoding/binary"
	"fmt"
	"net"
)

// rtpdumpFileHeader returns the text prefix line + 16-byte binary header
// in RTPtools rtpdump format.
//
// Binary header layout (big-endian):
//
//	uint32 start_sec   — Unix epoch seconds of recording start
//	uint32 start_usec  — microseconds part
//	uint32 source IP   — 4 bytes, network order
//	uint16 source port
//	uint16 padding (0)
func rtpdumpFileHeader(src string, port uint16, startSec, startUsec uint32) []byte {
	prefix := fmt.Sprintf("#!rtpplay1.0 %s/%d\n", src, port)
	bin := make([]byte, 16)
	binary.BigEndian.PutUint32(bin[0:], startSec)
	binary.BigEndian.PutUint32(bin[4:], startUsec)
	if ip := net.ParseIP(src).To4(); ip != nil {
		copy(bin[8:12], ip)
	}
	binary.BigEndian.PutUint16(bin[12:], port)
	binary.BigEndian.PutUint16(bin[14:], 0)
	return append([]byte(prefix), bin...)
}

// rtpdumpRecord returns an 8-byte record header followed by pkt.
//
// Record header layout (big-endian):
//
//	uint16 length   — plen + 8 (total record size)
//	uint16 plen     — len(pkt)
//	uint32 offset   — milliseconds since recording start
func rtpdumpRecord(pkt []byte, offsetMs uint32) []byte {
	plen := uint16(len(pkt))
	rec := make([]byte, 8+len(pkt))
	binary.BigEndian.PutUint16(rec[0:], plen+8)
	binary.BigEndian.PutUint16(rec[2:], plen)
	binary.BigEndian.PutUint32(rec[4:], offsetMs)
	copy(rec[8:], pkt)
	return rec
}
