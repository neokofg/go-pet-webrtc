package entities

import (
	"fmt"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"sync"
)

type MediaTrack struct {
	TrackLocal  *webrtc.TrackLocalStaticRTP
	Subscribers sync.Map
	OwnerID     string
	StreamID    string
	Type        string
	buffer      chan *rtp.Packet
}

func NewMediaTrack(codec webrtc.RTPCodecCapability, id, streamID, ownerID, trackType string) (*MediaTrack, error) {
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(codec, id, streamID)
	if err != nil {
		return nil, err
	}

	return &MediaTrack{
		TrackLocal: trackLocal,
		OwnerID:    ownerID,
		StreamID:   streamID,
		Type:       trackType,
		buffer:     make(chan *rtp.Packet, 512),
	}, nil
}

func (t *MediaTrack) processRTPPackets(remoteTrack *webrtc.TrackRemote) {
	fmt.Printf("Starting to process RTP packets for track: %s\n", remoteTrack.ID())

	go func() {
		for {
			buf := bufferPool.Get().([]byte)
			n, _, err := remoteTrack.Read(buf)
			if err != nil {
				fmt.Printf("Error reading RTP packet: %v\n", err)
				bufferPool.Put(buf)
				return
			}

			packet := &rtp.Packet{}
			if err := packet.Unmarshal(buf[:n]); err != nil {
				fmt.Printf("Error unmarshaling RTP packet: %v\n", err)
				bufferPool.Put(buf)
				continue
			}

			if err := t.TrackLocal.WriteRTP(packet); err != nil {
				fmt.Printf("Error writing RTP packet: %v\n", err)
			}

			bufferPool.Put(buf)
		}
	}()
}
