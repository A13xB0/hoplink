package bridge

import (
	"fmt"
	"regexp"
	"strings"
)

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
//
// A name that already ends in one or more origin tags — relayed by another
// hoplink, or a user who put "(MC)" in their own name — keeps only the
// innermost one, since that names where the message was really written,
// re-rendered in style. Without this every hop appended another tag:
// "Alice (DC) (MC) (MT)".
func formatSenderName(name string, o origin, style string) string {
	base, first, raw, tagged := stripOriginTags(name)
	if tagged {
		o = first
	}
	switch style {
	case "short":
		return fmt.Sprintf("%s (%s)", base, o.shortTag())
	case "full":
		return fmt.Sprintf("%s (%s)", base, o.fullTag())
	default:
		if tagged {
			return base + " " + raw // collapse a stack, but keep the tag as written
		}
		return name
	}
}

// trailingOriginTag matches one origin tag at the end of a sender name, in
// either style and any case, with () or [] and loose spacing: "Alice (MC)",
// "Alice(meshcore)", "Alice [DC]".
var trailingOriginTag = regexp.MustCompile(`(?i)\s*[(\[]\s*(dc|mc|mt|discord|meshcore|meshtastic)\s*[)\]]\s*$`)

// stripOriginTags removes every trailing origin tag from name, returning
// what's left and the innermost (first-applied) tag's origin and text. Other
// bracketed text, e.g. "Bob (Portable)", is part of the name and kept. A
// name that is nothing but a tag is returned unchanged.
func stripOriginTags(name string) (base string, first origin, raw string, tagged bool) {
	base = name
	for {
		loc := trailingOriginTag.FindStringSubmatchIndex(base)
		if loc == nil {
			break
		}
		rest := strings.TrimSpace(base[:loc[0]])
		if rest == "" {
			break
		}
		first, tagged = parseOriginTag(base[loc[2]:loc[3]]), true
		raw = strings.TrimSpace(base[loc[0]:])
		base = rest
	}
	return base, first, raw, tagged
}

func parseOriginTag(tag string) origin {
	switch strings.ToLower(tag) {
	case "dc", "discord":
		return originDiscord
	case "mc", "meshcore":
		return originMeshcore
	default:
		return originMeshtastic
	}
}
