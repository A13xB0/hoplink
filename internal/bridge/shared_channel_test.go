package bridge

import (
	"testing"
	"time"

	generated "github.com/meshnet-gophers/meshtastic-go/meshtastic"

	"github.com/hectospark/hoplink/internal/discord"
	"github.com/hectospark/hoplink/internal/meshcore"
)

// Two bridges sharing the same MeshCore channel (same hashtag/secret) but
// posting to different Discord channels — e.g. the same mesh channel
// bridged into two different guilds.

func TestBridge_HandleMeshcorePacket_PostsToAllBridgesSharingMeshcoreChannel(t *testing.T) {
	m1, posts1 := newTestMapping(t, "guild1", "#shared")
	m2, posts2 := newTestMapping(t, "guild2", "#shared")
	b := newTestBridge(m1, m2)

	lrx := buildLogRxData(t, m1.secret, 1000, "Alice: hello from the mesh")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts1:
		if p.content != "hello from the mesh" {
			t.Errorf("guild1 post = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for guild1's discord webhook post")
	}
	select {
	case p := <-posts2:
		if p.content != "hello from the mesh" {
			t.Errorf("guild2 post = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for guild2's discord webhook post")
	}
}

func TestBridge_HandleMeshcorePacket_SelfEchoSuppressesForAllSiblings(t *testing.T) {
	m1, posts1 := newTestMapping(t, "guild1", "#shared")
	m2, posts2 := newTestMapping(t, "guild2", "#shared")
	b := newTestBridge(m1, m2)

	b.markOutboundSent(meshcoreEchoKey(m1.channelHash, "Alice: hi"))

	lrx := buildLogRxData(t, m1.secret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)

	select {
	case p := <-posts1:
		t.Fatalf("expected self-echo to suppress guild1's post too, got %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case p := <-posts2:
		t.Fatalf("expected self-echo to suppress guild2's post, got %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_HandleMeshcorePacket_RelaysToMeshtasticOncePerSharedChannel(t *testing.T) {
	// Both sibling bridges relay to the SAME Meshtastic channel; the
	// physical send must happen exactly once, not twice.
	m1, _ := newTestMapping(t, "guild1", "#shared")
	m1.meshtasticEnabled = true
	m1.cfg.Meshtastic.Enabled = true
	m1.cfg.Meshtastic.ChannelName = "general"

	m2, _ := newTestMapping(t, "guild2", "#shared")
	m2.meshtasticEnabled = true
	m2.cfg.Meshtastic.Enabled = true
	m2.cfg.Meshtastic.ChannelName = "general"

	mtSession, mtPackets, _ := dialTestMeshtasticSession(t)
	b := newTestBridge(m1, m2)
	b.SetMeshtasticSession(mtSession)

	lrx := buildLogRxData(t, m1.secret, 1000, "Alice: hi")
	b.handleMeshcorePacket(lrx)

	select {
	case <-mtPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the meshcore->meshtastic relay")
	}
	select {
	case pkt := <-mtPackets:
		t.Fatalf("expected exactly one meshtastic send, got a second: %+v", pkt)
	case <-time.After(300 * time.Millisecond):
	}
}

// Symmetric scenario for Meshtastic: two bridges sharing the same
// meshtastic channel_name but posting to different Discord channels.

func TestBridge_HandleMeshtasticMessage_PostsToAllBridgesSharingMeshtasticChannel(t *testing.T) {
	mtSession, _, radio := dialTestMeshtasticSession(t)
	m1, posts1 := newTestMeshtasticMapping(t, "guild1", "chan-1", "general")
	m2, posts2 := newTestMeshtasticMapping(t, "guild2", "chan-2", "general")
	b := newTestBridge(m1, m2)
	b.SetMeshtasticSession(mtSession)

	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:    42,
		Channel: 1, // "general"
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hi from meshtastic"),
		}},
	}}})
	pumpOneTextMessage(t, b, mtSession)

	select {
	case p := <-posts1:
		if p.content != "hi from meshtastic" {
			t.Errorf("guild1 post = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for guild1's discord webhook post")
	}
	select {
	case p := <-posts2:
		if p.content != "hi from meshtastic" {
			t.Errorf("guild2 post = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for guild2's discord webhook post")
	}
}

func TestBridge_HandleMeshtasticMessage_RelaysToMeshcoreOncePerSharedChannel(t *testing.T) {
	mtSession, _, radio := dialTestMeshtasticSession(t)
	m1, _ := newTestMeshtasticMapping(t, "guild1", "chan-1", "general")
	m2, _ := newTestMeshtasticMapping(t, "guild2", "chan-2", "general")

	secret := meshcore.HashtagChannelSecret("#shared")
	chHash, err := meshcore.ChannelHash(secret)
	if err != nil {
		t.Fatalf("ChannelHash: %v", err)
	}
	for _, m := range []*mapping{m1, m2} {
		m.meshcoreEnabled = true
		m.cfg.MeshCore.Enabled = true
		m.cfg.MeshCore.Hashtag = "#shared"
		m.secret = secret
		m.channelHash = chHash
	}

	mcSession, mcPackets := dialTestSession(t)
	b := newTestBridge(m1, m2)
	b.SetMeshtasticSession(mtSession)
	b.SetMeshcoreSession(mcSession)
	b.route = meshcore.RouteFlood
	b.hashSize = 3

	radio.push(&generated.FromRadio{PayloadVariant: &generated.FromRadio_Packet{Packet: &generated.MeshPacket{
		From:    42,
		Channel: 1,
		PayloadVariant: &generated.MeshPacket_Decoded{Decoded: &generated.Data{
			Portnum: generated.PortNum_TEXT_MESSAGE_APP,
			Payload: []byte("hi"),
		}},
	}}})
	pumpOneTextMessage(t, b, mtSession)

	select {
	case <-mcPackets:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the meshtastic->meshcore relay")
	}
	select {
	case raw := <-mcPackets:
		t.Fatalf("expected exactly one meshcore send, got a second: %v", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

// Discord-to-Discord relay: a message typed in one bridge's Discord channel
// must directly (and immediately, with no dependency on RF loopback) show
// up in a sibling bridge's Discord channel when they share a mesh channel.

func TestBridge_HandleDiscordMessage_RelaysToSiblingSharingMeshcoreChannel(t *testing.T) {
	m1, _ := newTestMapping(t, "guild1", "#shared")
	m2, posts2 := newTestMapping(t, "guild2", "#shared")
	session, _ := dialTestSession(t)
	b := newTestBridge(m1, m2)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood
	b.hashSize = 3

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m1.cfg.DiscordChannelID,
		AuthorName: "Alice",
		Content:    "hello from guild1",
	})

	select {
	case p := <-posts2:
		if p.username != "Alice" || p.content != "hello from guild1" {
			t.Errorf("guild2 post = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the direct sibling discord relay")
	}
}

func TestBridge_HandleDiscordMessage_RelaysToSiblingSharingMeshtasticChannel(t *testing.T) {
	m1, _ := newTestMeshtasticMapping(t, "guild1", "chan-1", "general")
	m2, posts2 := newTestMeshtasticMapping(t, "guild2", "chan-2", "general")
	mtSession, _, _ := dialTestMeshtasticSession(t)
	b := newTestBridge(m1, m2)
	b.SetMeshtasticSession(mtSession)

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m1.cfg.DiscordChannelID,
		AuthorName: "Bob",
		Content:    "hello from guild1",
	})

	select {
	case p := <-posts2:
		if p.username != "Bob" || p.content != "hello from guild1" {
			t.Errorf("guild2 post = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the direct sibling discord relay")
	}
}

func TestBridge_HandleDiscordMessage_DoesNotRelayToUnrelatedBridge(t *testing.T) {
	m1, _ := newTestMapping(t, "guild1", "#shared")
	m3, posts3 := newTestMapping(t, "guild3", "#different")
	session, _ := dialTestSession(t)
	b := newTestBridge(m1, m3)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood
	b.hashSize = 3

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m1.cfg.DiscordChannelID,
		AuthorName: "Alice",
		Content:    "hello",
	})

	select {
	case p := <-posts3:
		t.Fatalf("expected no relay to an unrelated bridge, got %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// Read-only sides: a side marked read_only/discord_read_only must never
// transmit, only ever receive.

func TestBridge_SendMeshcore_ReadOnlySkipsTransmit(t *testing.T) {
	m, _ := newTestMapping(t, "guild1", "#shared")
	m.cfg.MeshCore.ReadOnly = true
	session, sentPackets := dialTestSession(t)
	b := newTestBridge(m)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood
	b.hashSize = 3

	b.sendMeshcore(m, "Alice", "hi")

	select {
	case raw := <-sentPackets:
		t.Fatalf("expected a read-only MeshCore side to never transmit, got a packet: %v", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_SendMeshtastic_ReadOnlySkipsTransmit(t *testing.T) {
	m, _ := newTestMeshtasticMapping(t, "guild1", "chan-1", "general")
	m.cfg.Meshtastic.ReadOnly = true
	mtSession, mtPackets, _ := dialTestMeshtasticSession(t)
	b := newTestBridge(m)
	b.SetMeshtasticSession(mtSession)

	b.sendMeshtastic(m, "Alice", "hi")

	select {
	case pkt := <-mtPackets:
		t.Fatalf("expected a read-only Meshtastic side to never transmit, got a packet: %+v", pkt)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridge_HandleDiscordMessage_DiscordReadOnlySkipsAllOutbound(t *testing.T) {
	m1, _ := newTestMapping(t, "guild1", "#shared")
	m1.cfg.DiscordReadOnly = true
	m2, posts2 := newTestMapping(t, "guild2", "#shared")
	session, sentPackets := dialTestSession(t)
	b := newTestBridge(m1, m2)
	b.SetMeshcoreSession(session)
	b.route = meshcore.RouteFlood
	b.hashSize = 3

	b.handleDiscordMessage(discord.IncomingMessage{
		ChannelID:  m1.cfg.DiscordChannelID,
		AuthorName: "Alice",
		Content:    "should never go anywhere",
	})

	select {
	case raw := <-sentPackets:
		t.Fatalf("expected discord_read_only to prevent any meshcore send, got a packet: %v", raw)
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case p := <-posts2:
		t.Fatalf("expected discord_read_only to prevent relaying to a sibling bridge, got %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}
