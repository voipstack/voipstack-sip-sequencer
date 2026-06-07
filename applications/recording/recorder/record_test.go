package recorder

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// fakeUAC is a minimal SIP UAC for test use.
type fakeUAC struct {
	dcc  *sipgo.DialogClientCache
	addr string
	l    net.Listener
}

func newFakeUAC(t *testing.T) *fakeUAC {
	t.Helper()
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("fakeUAC UA: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("fakeUAC client: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeUAC listen: %v", err)
	}
	addr := l.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	contact := sip.ContactHeader{Address: sip.Uri{Host: host, Port: port}}
	dcc := sipgo.NewDialogClientCache(cli, contact)
	t.Cleanup(func() { l.Close() })
	return &fakeUAC{dcc: dcc, addr: addr, l: l}
}

func (f *fakeUAC) invite(ctx context.Context, target string, offer []byte, extraHeaders ...sip.Header) (*sipgo.DialogClientSession, error) {
	var targetURI sip.Uri
	if err := sip.ParseUri(target, &targetURI); err != nil {
		return nil, err
	}
	params := targetURI.UriParams.Clone()
	params.Add("transport", "tcp")
	targetURI.UriParams = params
	return f.dcc.Invite(ctx, targetURI, offer, extraHeaders...)
}

// startRecorder starts Serve in a background goroutine and returns its listen address.
func startRecorder(t *testing.T, dir string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	cfg := Config{
		Listen:    addr,
		Dir:       dir,
		MediaHost: "127.0.0.1",
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = Serve(ctx, cfg) }()
	// Give the SIP server a moment to bind.
	time.Sleep(50 * time.Millisecond)
	return addr
}

// extractAnswerPorts parses m=audio port values from a SDP answer.
func extractAnswerPorts(sdp []byte) []int {
	var ports []int
	for _, rawLine := range bytes.Split(sdp, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				p, _ := strconv.Atoi(fields[1])
				if p > 0 {
					ports = append(ports, p)
				}
			}
		}
	}
	return ports
}

// sendUDP sends a single UDP datagram to addr.
func sendUDP(t *testing.T, addr string, pkt []byte) {
	t.Helper()
	dst, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}
	conn, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	if _, err := conn.Write(pkt); err != nil {
		t.Fatalf("send to %s: %v", addr, err)
	}
}

// minimalRTPPkt builds a minimal RTP packet with a distinct payload byte.
func minimalRTPPkt(marker byte) []byte {
	// RTP fixed header (12 bytes): V=2, no padding/ext, CC=0, M=0, PT=0
	hdr := []byte{0x80, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}
	return append(hdr, marker)
}

// Given a tap offer; When recorder answers and RTP arrives on both ports; Then
// caller.rtpdump and callee.rtpdump exist and contain the sent packets (AC1+AC2).
func TestRecordsBothDirectionsToFolder(t *testing.T) {
	dir := t.TempDir()
	recorderAddr := startRecorder(t, dir)

	uac := newFakeUAC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := uac.invite(ctx, "sip:"+recorderAddr, []byte(tapOffer))
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}

	answer := sess.InviteResponse.Body()
	ports := extractAnswerPorts(answer)
	if len(ports) < 2 {
		t.Fatalf("want 2 ports in answer, got %d: %s", len(ports), answer)
	}

	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	callerPkt := minimalRTPPkt(0xCA)
	calleePkt := minimalRTPPkt(0xCB)
	sendUDP(t, "127.0.0.1:"+strconv.Itoa(ports[0]), callerPkt)
	sendUDP(t, "127.0.0.1:"+strconv.Itoa(ports[1]), calleePkt)

	// Give recording goroutines time to process the packets.
	time.Sleep(50 * time.Millisecond)

	if err := sess.Bye(ctx); err != nil {
		t.Fatalf("BYE: %v", err)
	}
	// Wait for sockets to close and files to flush.
	time.Sleep(100 * time.Millisecond)

	callID := sess.InviteRequest.CallID().Value()
	callDir := filepath.Join(dir, sanitizeName(callID))

	for _, name := range []string{"caller", "callee"} {
		path := filepath.Join(callDir, name+".rtpdump")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		pkts, err := decodeRtpdump(data)
		if err != nil {
			t.Errorf("decode %s: %v", path, err)
			continue
		}
		if len(pkts) == 0 {
			t.Errorf("%s: want at least 1 packet, got 0", name)
		}
	}

	// Verify caller stream got the caller packet, callee stream got the callee packet.
	checkStream := func(filename string, wantMarker byte) {
		path := filepath.Join(callDir, filename)
		data, _ := os.ReadFile(path)
		pkts, _ := decodeRtpdump(data)
		if len(pkts) == 0 {
			t.Errorf("%s: no packets", filename)
			return
		}
		// Last byte of our minimal packet is the marker.
		if pkts[0][len(pkts[0])-1] != wantMarker {
			t.Errorf("%s: want last byte 0x%02X, got 0x%02X", filename, wantMarker, pkts[0][len(pkts[0])-1])
		}
	}
	checkStream("caller.rtpdump", 0xCA)
	checkStream("callee.rtpdump", 0xCB)
}

// Given an INVITE with X-Sequencer-Call-Id header; When call ends;
// Then the recording folder is named after the header value.
func TestFolderNamedBySequencerCallId(t *testing.T) {
	dir := t.TempDir()
	recorderAddr := startRecorder(t, dir)

	uac := newFakeUAC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	seqCallID := "seq-test-abc-123"
	sess, err := uac.invite(ctx, "sip:"+recorderAddr, []byte(tapOffer),
		sip.NewHeader("X-Sequencer-Call-Id", seqCallID))
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	if err := sess.Bye(ctx); err != nil {
		t.Fatalf("BYE: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	expectedDir := filepath.Join(dir, seqCallID)
	if _, err := os.Stat(expectedDir); os.IsNotExist(err) {
		t.Errorf("want folder %s, not found", expectedDir)
	}
}

// Given an INVITE without X-Sequencer-Call-Id; When call ends;
// Then the folder is named after the SIP Call-ID.
func TestFolderFallsBackToSipCallID(t *testing.T) {
	dir := t.TempDir()
	recorderAddr := startRecorder(t, dir)

	uac := newFakeUAC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := uac.invite(ctx, "sip:"+recorderAddr, []byte(tapOffer))
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	callID := sess.InviteRequest.CallID().Value()

	if err := sess.Bye(ctx); err != nil {
		t.Fatalf("BYE: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	expectedDir := filepath.Join(dir, sanitizeName(callID))
	if _, err := os.Stat(expectedDir); os.IsNotExist(err) {
		t.Errorf("want folder %s (from SIP Call-ID), not found", expectedDir)
	}
}
