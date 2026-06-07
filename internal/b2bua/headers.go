package b2bua

import (
	"strings"

	"github.com/emiago/sipgo/sip"
)

// A B2BUA relays each call as two correlated-but-independent dialogs. To stay
// transparent toward both sides it copies every header it does not own — caller
// identity (From/To), auth (Authorization, WWW-Authenticate, …) and any custom
// X-* business headers — verbatim across the bridge, rewriting only the headers a
// B2BUA must own per leg.

// requestOwnedHeaders are sequencer/sipgo-owned per outbound leg; everything else on
// the inbound INVITE is relayed verbatim onto the leg so it looks like the original
// request to its target.
var requestOwnedHeaders = map[string]bool{
	"via":            true,
	"call-id":        true,
	"cseq":           true,
	"max-forwards":   true,
	"contact":        true,
	"route":          true,
	"record-route":   true,
	"content-type":   true,
	"content-length": true,
}

// responseOwnedHeaders are owned by the inbound dialog/transaction; everything else
// on an upstream leg response (auth challenges, Retry-After, Warning, custom X-*) is
// relayed verbatim back to the endpoint. Via/From/To/Call-ID/CSeq/Contact must stay
// the inbound dialog's — sipgo derives the dialog id from them and owns the Contact
// so in-dialog requests keep traversing the B2BUA.
var responseOwnedHeaders = map[string]bool{
	"via":            true,
	"from":           true,
	"to":             true,
	"call-id":        true,
	"cseq":           true,
	"contact":        true,
	"route":          true,
	"record-route":   true,
	"timestamp":      true,
	"content-type":   true,
	"content-length": true,
}

// sequencerHeaderPrefix marks correlation headers the sequencer mints itself; they
// are never copied from a peer (in either direction) so a peer cannot spoof or leak
// internal correlation ids.
const sequencerHeaderPrefix = "x-sequencer-"

type headerCarrier interface{ Headers() []sip.Header }

// relayableHeaders returns fresh clones of every header on msg whose name is neither
// owned by the B2BUA nor a sequencer-minted correlation header. Cloning keeps the
// relayed headers independent of the source message and of each other (no shared
// mutable params).
func relayableHeaders(msg headerCarrier, owned map[string]bool) []sip.Header {
	src := msg.Headers()
	out := make([]sip.Header, 0, len(src))
	for _, h := range src {
		name := strings.ToLower(h.Name())
		if owned[name] || strings.HasPrefix(name, sequencerHeaderPrefix) {
			continue
		}
		out = append(out, sip.HeaderClone(h))
	}
	return out
}

// relayableResponseHeaders returns the relayable headers from an upstream SIP response,
// and ensures Content-Type is present when the outgoing response carries a body.
// sipgo's Respond path builds responses with NewResponseFromRequest which sets
// body/content-length but does not add Content-Type; the response-owned filter
// strips it, so it must be re-added explicitly when the body is non-empty.
func relayableResponseHeaders(upstream *sip.Response, body []byte) []sip.Header {
	hdrs := relayableHeaders(upstream, responseOwnedHeaders)
	if len(body) == 0 {
		return hdrs
	}
	if ct := upstream.GetHeader("Content-Type"); ct != nil {
		hdrs = append(hdrs, sip.HeaderClone(ct))
	} else {
		hdrs = append(hdrs, sip.NewHeader("Content-Type", "application/sdp"))
	}
	return hdrs
}
