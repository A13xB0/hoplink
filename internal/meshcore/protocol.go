// Package meshcore implements the MeshCore companion radio protocol: TCP
// serial framing, command/response handshake, and raw RF packet
// construction/parsing for channel (hashtag) chat.
//
// Reference: https://github.com/meshcore-dev/MeshCore
// docs/companion_protocol.md and docs/packet_format.md.
package meshcore

// Command opcodes sent from app to radio (companion protocol).
const (
	CmdAppStart          = 0x01
	CmdSendTxtMsg        = 0x02
	CmdSendChannelTxt    = 0x03
	CmdGetContacts       = 0x04
	CmdGetDeviceTime     = 0x05
	CmdSetDeviceTime     = 0x06
	CmdSendSelfAdvert    = 0x07
	CmdSetAdvertName     = 0x08
	CmdSyncNextMessage   = 0x0A
	CmdDeviceQuery       = 0x16
	CmdSendRawData       = 0x19
	CmdGetChannel        = 0x1F
	CmdSetChannel        = 0x20
	CmdSendRawPacket     = 0x41 // 65 — CMD_SEND_RAW_PACKET (firmware v1.16.0+)
	CmdGetBattAndStorage = 0x14
)

// Response / push first-byte codes (companion protocol).
const (
	FrameOK               = 0x00
	FrameErr              = 0x01
	FrameContactsStart    = 0x02
	FrameContact          = 0x03
	FrameEndOfContacts    = 0x04
	FrameSelfInfo         = 0x05
	FrameSent             = 0x06
	FrameContactMsgRecv   = 0x07
	FrameChannelMsgRecv   = 0x08
	FrameCurrTime         = 0x09
	FrameNoMoreMessages   = 0x0A
	FrameBattery          = 0x0C
	FrameDeviceInfo       = 0x0D
	FrameChannelInfo      = 0x12
	FrameContactMsgRecvV3 = 0x10
	FrameChannelMsgRecvV3 = 0x11
	FrameChannelDataRecv  = 0x1B
	PushAdvert            = 0x80
	PushAck               = 0x82
	PushMsgWaiting        = 0x83
	PushRawData           = 0x84
	PushLoginSuccess      = 0x85
	PushLoginFail         = 0x86
	PushStatusResponse    = 0x87
	PushLogRxData         = 0x88 // PUSH_CODE_LOG_RX_DATA — raw RF receive log
	PushTraceData         = 0x89
	PushNewAdvert         = 0x8A
	PushTelemetryResponse = 0x8B
	PushBinaryResponse    = 0x8C
	PushPathDiscoveryResp = 0x8D
)

// ErrCode values carried in byte 1 of a FrameErr (0x01) response.
const (
	ErrCodeUnsupportedCmd = 1
	ErrCodeNotFound       = 2
	ErrCodeTableFull      = 3
	ErrCodeBadState       = 4
	ErrCodeFileIOError    = 5
	ErrCodeIllegalArg     = 6
)

// RfRouteType is the RF packet header route type (header bits [1:0]).
type RfRouteType byte

const (
	RouteTransportFlood  RfRouteType = 0x00
	RouteFlood           RfRouteType = 0x01
	RouteDirect          RfRouteType = 0x02
	RouteTransportDirect RfRouteType = 0x03
)

// HasTransportCodes reports whether this route type carries the 4-byte
// transport_codes field (only TRANSPORT_FLOOD and TRANSPORT_DIRECT do).
func (r RfRouteType) HasTransportCodes() bool {
	return r == RouteTransportFlood || r == RouteTransportDirect
}

// WithTransportCodes returns the TRANSPORT_* variant of a FLOOD or DIRECT
// route — used when scoping a packet to a named flood scope/region via
// CalcTransportCode. Other route types are returned unchanged.
func (r RfRouteType) WithTransportCodes() RfRouteType {
	switch r {
	case RouteFlood:
		return RouteTransportFlood
	case RouteDirect:
		return RouteTransportDirect
	default:
		return r
	}
}

// RfPayloadType is the RF packet header payload type (header bits [5:2]).
type RfPayloadType byte

const (
	PayloadTypeReq       RfPayloadType = 0x00
	PayloadTypeResponse  RfPayloadType = 0x01
	PayloadTypeTxtMsg    RfPayloadType = 0x02
	PayloadTypeAck       RfPayloadType = 0x03
	PayloadTypeAdvert    RfPayloadType = 0x04
	PayloadTypeGrpTxt    RfPayloadType = 0x05 // group/channel text message
	PayloadTypeGrpData   RfPayloadType = 0x06
	PayloadTypeAnonReq   RfPayloadType = 0x07
	PayloadTypePath      RfPayloadType = 0x08
	PayloadTypeTrace     RfPayloadType = 0x09
	PayloadTypeMultipart RfPayloadType = 0x0A
	PayloadTypeControl   RfPayloadType = 0x0B
	PayloadTypeRawCustom RfPayloadType = 0x0F
)

// RfPayloadVer is the RF packet header payload version (header bits [7:6]).
type RfPayloadVer byte

const (
	PayloadVer1 RfPayloadVer = 0x00
	PayloadVer2 RfPayloadVer = 0x01
	PayloadVer3 RfPayloadVer = 0x02
	PayloadVer4 RfPayloadVer = 0x03
)

// Protocol size limits (src/MeshCore.h).
const (
	MaxPacketPayload = 184 // MAX_PACKET_PAYLOAD
	MaxPathSize      = 64  // MAX_PATH_SIZE
	MaxFrameSize     = 176 // MAX_FRAME_SIZE
	// MaxRawPacketLen is the largest serialised packet that fits a
	// CMD_SEND_RAW_PACKET companion frame: MaxFrameSize - [cmd] - [priority].
	MaxRawPacketLen = MaxFrameSize - 2
)
