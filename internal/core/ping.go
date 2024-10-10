package msg

import "code.dogecoin.org/gossip/codec"

type PingMsg struct {
	Nonce uint64 // random nonce
}

func DecodePing(payload []byte) (msg PingMsg) {
	d := codec.Decode(payload)
	msg.Nonce = d.UInt64le()
	return
}

func EncodePing(msg PingMsg) []byte {
	e := codec.Encode(86)
	e.UInt64le(msg.Nonce)
	return e.Result()
}
