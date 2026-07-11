package meshcore

import (
	"fmt"
)

// Packet is a parsed MeshCore RF packet:
//
//	[header][transport_codes (4, optional)][path_len][path][payload]
//
// See docs/packet_format.md in the meshcore firmware repo.
type Packet struct {
	Route          RfRouteType
	PayloadType    RfPayloadType
	Version        RfPayloadVer
	TransportCodes []byte // exactly 4 bytes when Route.HasTransportCodes()
	Path           []byte // hopCount * hashSize bytes
	HashSize       int    // bytes per hop hash (1-4); meaningful only if len(Path) > 0
	Payload        []byte
}

// BuildPacket serialises p to its on-wire bytes (mirrors firmware
// Packet::writeTo / MeshCIM's CompanionCodec.buildPacket).
func BuildPacket(p Packet) ([]byte, error) {
	hashSize := p.HashSize
	if hashSize == 0 {
		hashSize = 1
	}
	if hashSize < 1 || hashSize > 4 {
		return nil, fmt.Errorf("meshcore: hashSize must be 1..4, got %d", hashSize)
	}
	if len(p.Path)%hashSize != 0 {
		return nil, fmt.Errorf("meshcore: path length %d not a multiple of hashSize %d", len(p.Path), hashSize)
	}
	hopCount := len(p.Path) / hashSize
	if hopCount > 63 {
		return nil, fmt.Errorf("meshcore: hopCount %d exceeds 63", hopCount)
	}
	if len(p.Path) > MaxPathSize {
		return nil, fmt.Errorf("meshcore: path length %d exceeds MaxPathSize %d", len(p.Path), MaxPathSize)
	}
	if len(p.Payload) > MaxPacketPayload {
		return nil, fmt.Errorf("meshcore: payload length %d exceeds MaxPacketPayload %d", len(p.Payload), MaxPacketPayload)
	}
	hasTransport := p.Route.HasTransportCodes()
	if hasTransport && len(p.TransportCodes) != 4 {
		return nil, fmt.Errorf("meshcore: route %d requires exactly 4 transport code bytes, got %d", p.Route, len(p.TransportCodes))
	}

	header := byte(p.Route&0x03) | (byte(p.PayloadType&0x0F) << 2) | (byte(p.Version&0x03) << 6)
	pathLen := byte((hashSize-1)<<6) | byte(hopCount&0x3F)

	out := make([]byte, 0, 2+len(p.TransportCodes)+len(p.Path)+len(p.Payload))
	out = append(out, header)
	if hasTransport {
		out = append(out, p.TransportCodes...)
	}
	out = append(out, pathLen)
	out = append(out, p.Path...)
	out = append(out, p.Payload...)
	return out, nil
}

// ParsePacket decodes raw RF packet bytes (the same wire format BuildPacket
// produces) back into a Packet. Used to interpret PUSH_CODE_LOG_RX_DATA
// (0x88) raw packet bytes.
func ParsePacket(raw []byte) (Packet, error) {
	if len(raw) < 2 {
		return Packet{}, fmt.Errorf("meshcore: packet too short (%d bytes)", len(raw))
	}
	header := raw[0]
	route := RfRouteType(header & 0x03)
	payloadType := RfPayloadType((header >> 2) & 0x0F)
	version := RfPayloadVer((header >> 6) & 0x03)

	off := 1
	var transportCodes []byte
	if route.HasTransportCodes() {
		if len(raw) < off+4+1 {
			return Packet{}, fmt.Errorf("meshcore: packet too short for transport codes")
		}
		transportCodes = append([]byte(nil), raw[off:off+4]...)
		off += 4
	}

	if off >= len(raw) {
		return Packet{}, fmt.Errorf("meshcore: packet missing path_len byte")
	}
	pathLenByte := raw[off]
	off++
	hashSize := int((pathLenByte>>6)&0x03) + 1
	hopCount := int(pathLenByte & 0x3F)
	pathBytes := hopCount * hashSize
	if off+pathBytes > len(raw) {
		return Packet{}, fmt.Errorf("meshcore: packet truncated in path (need %d bytes)", pathBytes)
	}
	var path []byte
	if pathBytes > 0 {
		path = append([]byte(nil), raw[off:off+pathBytes]...)
	}
	off += pathBytes

	payload := append([]byte(nil), raw[off:]...)

	return Packet{
		Route:          route,
		PayloadType:    payloadType,
		Version:        version,
		TransportCodes: transportCodes,
		Path:           path,
		HashSize:       hashSize,
		Payload:        payload,
	}, nil
}

// BuildSendRawPacketFrame wraps a serialised packet in a CMD_SEND_RAW_PACKET
// (0x41/65) companion frame: [65][priority][packet]. The radio transmits the
// packet verbatim.
func BuildSendRawPacketFrame(packet []byte, priority byte) ([]byte, error) {
	if len(packet) > MaxRawPacketLen {
		return nil, fmt.Errorf("meshcore: serialised packet length %d exceeds companion frame budget %d", len(packet), MaxRawPacketLen)
	}
	out := make([]byte, 2+len(packet))
	out[0] = CmdSendRawPacket
	out[1] = priority
	copy(out[2:], packet)
	return out, nil
}

// LogRxData is a parsed PUSH_CODE_LOG_RX_DATA (0x88) frame:
// [0x88][snr×4 int8][rssi int8][raw packet bytes].
type LogRxData struct {
	SNR    float64 // dB
	RSSI   int8
	Packet Packet
}

// ParseLogRxData parses a raw 0x88 push frame into its SNR/RSSI header and
// decoded RF packet.
func ParseLogRxData(frame []byte) (LogRxData, error) {
	if len(frame) < 3 || frame[0] != PushLogRxData {
		return LogRxData{}, fmt.Errorf("meshcore: not a PUSH_CODE_LOG_RX_DATA frame")
	}
	snrRaw := int8(frame[1])
	rssi := int8(frame[2])
	pkt, err := ParsePacket(frame[3:])
	if err != nil {
		return LogRxData{}, fmt.Errorf("meshcore: parsing 0x88 packet: %w", err)
	}
	return LogRxData{
		SNR:    float64(snrRaw) / 4.0,
		RSSI:   rssi,
		Packet: pkt,
	}, nil
}
