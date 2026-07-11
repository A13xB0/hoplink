package bridge

import "fmt"

// origin identifies which surface a relayed message came from, so the
// destination(s) it's relayed to can label it per the configured
// sender_format.
type origin int

const (
	originDiscord origin = iota
	originMeshcore
	originMeshtastic
)

func (o origin) shortTag() string {
	switch o {
	case originDiscord:
		return "DC"
	case originMeshcore:
		return "MC"
	case originMeshtastic:
		return "MT"
	default:
		return "?"
	}
}

func (o origin) fullTag() string {
	switch o {
	case originDiscord:
		return "Discord"
	case originMeshcore:
		return "MeshCore"
	case originMeshtastic:
		return "Meshtastic"
	default:
		return "?"
	}
}

// formatSenderName returns name as it should be displayed on a destination
// other than o, per style ("none", "short", or "full" — any other value,
// including "", is treated as "none"). Used wherever a message crosses from
// one surface (Discord/MeshCore/Meshtastic) to another.
func formatSenderName(name string, o origin, style string) string {
	switch style {
	case "short":
		return fmt.Sprintf("%s (%s)", name, o.shortTag())
	case "full":
		return fmt.Sprintf("%s (%s)", name, o.fullTag())
	default:
		return name
	}
}
