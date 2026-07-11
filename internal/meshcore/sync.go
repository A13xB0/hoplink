package meshcore

import (
	"encoding/binary"
	"fmt"
)

// MaxChannelSlots is the highest valid channel index (0-7): firmware stores
// channels in a fixed-size table (BaseChatMesh::channels[MAX_GROUP_CHANNELS]).
// Index 0 is conventionally reserved for the public channel by the official
// apps/docs (docs/companion_protocol.md); the firmware itself does not
// special-case it, but this package follows the same convention so it never
// collides with another tool's use of slot 0.
const MaxChannelSlots = 7

// channelNameFieldLen is CMD_SET_CHANNEL/RESP_CODE_CHANNEL_INFO's fixed
// 32-byte name field (examples/companion_radio/MyMesh.cpp: `strcpy(&out_frame[i],
// channel.name); i += 32`). The name is a null-terminated C string within
// this field, so the usable length is 31 bytes to guarantee room for the
// terminator.
const channelNameFieldLen = 32

// BuildGetChannelFrame builds a CMD_GET_CHANNEL (0x1F) command frame
// requesting the channel registered at index (0-7).
func BuildGetChannelFrame(index byte) []byte {
	return []byte{CmdGetChannel, index}
}

// BuildSetChannelFrame builds a CMD_SET_CHANNEL (0x20) command frame
// registering name/secret16 at index (0-7). Matches firmware's exact parsing
// (examples/companion_radio/MyMesh.cpp): [0x20][index][name, 32 bytes,
// null-padded][secret, 16 bytes] — 50 bytes total.
func BuildSetChannelFrame(index byte, name string, secret16 []byte) ([]byte, error) {
	if len(secret16) != 16 {
		return nil, fmt.Errorf("meshcore: channel secret must be exactly 16 bytes, got %d", len(secret16))
	}
	nameBytes := []byte(name)
	if len(nameBytes) > channelNameFieldLen-1 {
		return nil, fmt.Errorf("meshcore: channel name %q exceeds %d bytes", name, channelNameFieldLen-1)
	}

	out := make([]byte, 2+channelNameFieldLen+16)
	out[0] = CmdSetChannel
	out[1] = index
	copy(out[2:2+channelNameFieldLen], nameBytes) // remainder stays zero: null-terminated + padded
	copy(out[2+channelNameFieldLen:], secret16)
	return out, nil
}

// ChannelInfo is the parsed reply to CMD_GET_CHANNEL.
type ChannelInfo struct {
	Index  byte
	Name   string
	Secret []byte // 16 bytes; all-zero for a never-configured slot
}

// ParseChannelInfo parses a RESP_CODE_CHANNEL_INFO (0x12) frame: [0x12]
// [index][name, 32 bytes, null-terminated][secret, 16 bytes] — 50 bytes
// total, byte-exact match to firmware's writer
// (examples/companion_radio/MyMesh.cpp).
func ParseChannelInfo(frame []byte) (ChannelInfo, bool) {
	const wantLen = 2 + channelNameFieldLen + 16
	if len(frame) < wantLen || frame[0] != FrameChannelInfo {
		return ChannelInfo{}, false
	}
	nameField := frame[2 : 2+channelNameFieldLen]
	nameEnd := len(nameField)
	for i, b := range nameField {
		if b == 0 {
			nameEnd = i
			break
		}
	}
	secret := append([]byte(nil), frame[2+channelNameFieldLen:2+channelNameFieldLen+16]...)
	return ChannelInfo{
		Index:  frame[1],
		Name:   string(nameField[:nameEnd]),
		Secret: secret,
	}, true
}

// IsEmptyChannelSlot reports whether info represents a never-configured slot
// (per docs/companion_protocol.md: "Fetch all channel slots, and find one
// with empty name and all-zero secret").
func (info ChannelInfo) IsEmptyChannelSlot() bool {
	if info.Name != "" {
		return false
	}
	for _, b := range info.Secret {
		if b != 0 {
			return false
		}
	}
	return true
}

// BuildSyncNextMessageFrame builds a CMD_SYNC_NEXT_MESSAGE (0x0A) command
// frame requesting the next queued message from the device.
func BuildSyncNextMessageFrame() []byte {
	return []byte{CmdSyncNextMessage}
}

// ChannelMsgRecv is a parsed PACKET_CHANNEL_MSG_RECV (0x08) or its V3
// variant (0x11) — a channel/group text message the device already
// decrypted for us, retrieved via CMD_SYNC_NEXT_MESSAGE.
type ChannelMsgRecv struct {
	ChannelIndex  byte
	TimestampUnix uint32
	Text          string
}

// ParseChannelMsgRecv parses a standard (0x08) or V3 (0x11)
// PACKET_CHANNEL_MSG_RECV frame:
//
//	Standard: [0x08][channel_idx][path_len][txt_type][timestamp LE32][text...]
//	V3:       [0x11][snr int8][2 reserved][channel_idx][path_len][txt_type][timestamp LE32][text...]
func ParseChannelMsgRecv(frame []byte) (ChannelMsgRecv, bool) {
	if len(frame) == 0 {
		return ChannelMsgRecv{}, false
	}
	off := 1
	switch frame[0] {
	case FrameChannelMsgRecv:
		// off already past the single type byte
	case FrameChannelMsgRecvV3:
		off += 3 // snr(1) + reserved(2)
	default:
		return ChannelMsgRecv{}, false
	}
	// off..: channel_idx(1) path_len(1) txt_type(1) timestamp(4) text...
	if len(frame) < off+7 {
		return ChannelMsgRecv{}, false
	}
	channelIdx := frame[off]
	timestamp := binary.LittleEndian.Uint32(frame[off+3 : off+7])
	text := string(frame[off+7:])
	return ChannelMsgRecv{
		ChannelIndex:  channelIdx,
		TimestampUnix: timestamp,
		Text:          text,
	}, true
}
