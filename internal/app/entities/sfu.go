package entities

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"net/http"
	"sync"
)

var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 1500)
	},
}

var optimizedPeerConfig = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
		{
			URLs: []string{"stun:stun1.l.google.com:19302"},
		},
	},
	ICETransportPolicy: webrtc.ICETransportPolicyAll,
	BundlePolicy:       webrtc.BundlePolicyMaxBundle,
	RTCPMuxPolicy:      webrtc.RTCPMuxPolicyRequire,
}

type SFUServer struct {
	rooms    sync.Map
	upgrader websocket.Upgrader
}

func NewSFUServer() *SFUServer {
	return &SFUServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

func createInterceptor() (*interceptor.Registry, error) {
	registry := &interceptor.Registry{}

	mediaEngine := &webrtc.MediaEngine{}

	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    "video/H264",
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  "audio/opus",
			ClockRate: 48000,
			Channels:  2,
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}

	if err := webrtc.ConfigureNack(mediaEngine, registry); err != nil {
		return nil, err
	}

	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return nil, err
	}

	return registry, nil
}

func createOptimizedPeerConnection() (*webrtc.PeerConnection, error) {
	settingEngine := webrtc.SettingEngine{}

	// Используем публичный IP вместо localhost
	settingEngine.SetNAT1To1IPs([]string{"94.235.229.134"}, webrtc.ICECandidateTypeHost)

	i, err := createInterceptor()
	if err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(
		webrtc.WithSettingEngine(settingEngine),
		webrtc.WithInterceptorRegistry(i),
	)

	return api.NewPeerConnection(optimizedPeerConfig)
}

func (s *SFUServer) getOrCreateRoom(roomID string) *Room {
	value, _ := s.rooms.LoadOrStore(roomID, NewRoom(roomID))
	return value.(*Room)
}

func (s *SFUServer) HandleWebSocket(c *gin.Context) {
	ws, err := s.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer ws.Close()

	participantID := c.Query("id")
	roomID := c.Query("room")

	if participantID == "" || roomID == "" {
		ws.WriteJSON(WebSocketMessage{
			Event: "error",
			Data:  "Both ID and room are required",
		})
		return
	}

	room := s.getOrCreateRoom(roomID)
	participant, err := s.addParticipantToRoom(participantID, roomID, ws)
	if err != nil {
		ws.WriteJSON(WebSocketMessage{
			Event: "error",
			Data:  err.Error(),
		})
		return
	}
	defer s.removeParticipantFromRoom(participantID, roomID)

	s.sendExistingTracks(participant, room)

	for {
		var msg WebSocketMessage
		err := ws.ReadJSON(&msg)
		if err != nil {
			break
		}

		switch msg.Event {
		case "offer":
			handleOffer(participant, msg.Data)
		case "candidate":
			handleCandidate(participant, msg.Data)
		case "subscribe":
			trackID, ok := msg.Data.(string)
			if ok {
				s.handleTrackSubscription(room, participant, trackID, true)
			}
		case "unsubscribe":
			trackID, ok := msg.Data.(string)
			if ok {
				s.handleTrackSubscription(room, participant, trackID, false)
			}
		}
	}
}

func (s *SFUServer) addParticipantToRoom(participantID, roomID string, ws *websocket.Conn) (*Participant, error) {
	pc, err := createOptimizedPeerConnection()
	if err != nil {
		return nil, fmt.Errorf("failed to create peer connection: %v", err)
	}

	participant := &Participant{
		pc: pc,
		ws: ws,
		id: participantID,
	}

	if err := setupPeerConnection(participant, s, roomID); err != nil {
		return nil, err
	}

	room := s.getOrCreateRoom(roomID)
	shard := room.getParticipantShard(participantID)

	shard.mu.Lock()
	shard.participants[participantID] = participant
	shard.mu.Unlock()

	s.broadcastInRoom(roomID, participantID, WebSocketMessage{
		Event: "participant_joined",
		Data: map[string]string{
			"participantID": participantID,
		},
	})

	return participant, nil
}

func (s *SFUServer) removeParticipantFromRoom(participantID, roomID string) {
	roomVal, ok := s.rooms.Load(roomID)
	if !ok {
		return
	}
	room := roomVal.(*Room)

	shard := room.getParticipantShard(participantID)
	shard.mu.Lock()
	if participant, ok := shard.participants[participantID]; ok {
		participant.pc.Close()
		participant.ws.Close()
		delete(shard.participants, participantID)
	}
	shard.mu.Unlock()

	// Проверяем, есть ли еще участники в комнате
	isEmpty := true
	for _, shard := range room.participantShards {
		shard.mu.RLock()
		if len(shard.participants) > 0 {
			isEmpty = false
		}
		shard.mu.RUnlock()
		if !isEmpty {
			break
		}
	}

	// Если комната пуста - удаляем её
	if isEmpty {
		s.rooms.Delete(roomID)
	} else {
		// Иначе уведомляем остальных участников
		s.broadcastInRoom(roomID, participantID, WebSocketMessage{
			Event: "participant_left",
			Data: map[string]string{
				"participantID": participantID,
			},
		})
	}
}

func (s *SFUServer) broadcastInRoom(roomID, excludeID string, msg WebSocketMessage) {
	roomVal, ok := s.rooms.Load(roomID)
	if !ok {
		return
	}
	room := roomVal.(*Room)

	for _, shard := range room.participantShards {
		shard.mu.RLock()
		for pid, participant := range shard.participants {
			if pid != excludeID {
				participant.ws.WriteJSON(msg)
			}
		}
		shard.mu.RUnlock()
	}
}

func setupPeerConnection(p *Participant, s *SFUServer, roomID string) error {
	p.pc.OnNegotiationNeeded(func() {
		fmt.Println("Negotiation needed, creating new offer")

		offer, err := p.pc.CreateOffer(nil)
		if err != nil {
			fmt.Printf("Error creating offer: %v\n", err)
			return
		}

		if err = p.pc.SetLocalDescription(offer); err != nil {
			fmt.Printf("Error setting local description: %v\n", err)
			return
		}

		offerBytes, err := json.Marshal(offer)
		if err != nil {
			fmt.Printf("Error marshaling offer: %v\n", err)
			return
		}

		p.ws.WriteJSON(WebSocketMessage{
			Event: "offer",
			Data:  string(offerBytes),
		})
	})

	p.pc.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		fmt.Printf("New track received: ID=%s, Kind=%s\n", remoteTrack.ID(), remoteTrack.Kind().String())

		trackType := remoteTrack.Kind().String()
		mediaTrack, err := NewMediaTrack(
			remoteTrack.Codec().RTPCodecCapability,
			remoteTrack.ID(),
			remoteTrack.StreamID(),
			p.id,
			trackType,
		)
		if err != nil {
			fmt.Printf("Error creating MediaTrack: %v\n", err)
			return
		}

		// Сохраняем трек
		room := s.getOrCreateRoom(roomID)
		room.tracksMap.Store(remoteTrack.ID(), mediaTrack)

		// Отправляем уведомление
		notification := WebSocketMessage{
			Event: "new_track",
			Data: map[string]string{
				"trackID":   remoteTrack.ID(),
				"ownerID":   p.id,
				"streamID":  remoteTrack.StreamID(),
				"trackType": trackType,
			},
		}
		notificationJSON, _ := json.Marshal(notification)
		fmt.Printf("Sending track notification: %s\n", string(notificationJSON))

		s.broadcastInRoom(roomID, p.id, notification)

		// Запускаем обработку RTP
		mediaTrack.processRTPPackets(remoteTrack)
	})

	return nil
}

func handleOffer(p *Participant, data interface{}) {
	sdp, ok := data.(string)
	if !ok {
		return
	}

	offer := webrtc.SessionDescription{}
	if err := json.Unmarshal([]byte(sdp), &offer); err != nil {
		fmt.Printf("Error unmarshaling offer: %v\n", err)
		return
	}

	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return
	}

	p.mu.Lock()
	for _, candidate := range p.pendingCandidates {
		p.pc.AddICECandidate(candidate)
	}
	p.pendingCandidates = nil
	p.mu.Unlock()

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		fmt.Printf("Error creating answer: %v\n", err)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(p.pc)

	if err := p.pc.SetLocalDescription(answer); err != nil {
		fmt.Printf("Error setting local description: %v\n", err)
		return
	}

	<-gatherComplete

	answerBytes, err := json.Marshal(*p.pc.LocalDescription())
	if err != nil {
		fmt.Printf("Error marshaling answer: %v\n", err)
		return
	}

	p.ws.WriteJSON(WebSocketMessage{
		Event: "answer",
		Data:  string(answerBytes),
	})
}

func handleCandidate(p *Participant, data interface{}) {
	candidateStr, ok := data.(string)
	if !ok {
		return
	}

	candidate := webrtc.ICECandidateInit{}
	if err := json.Unmarshal([]byte(candidateStr), &candidate); err != nil {
		return
	}

	p.mu.Lock()
	if p.pc.RemoteDescription() == nil {
		p.pendingCandidates = append(p.pendingCandidates, candidate)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	if err := p.pc.AddICECandidate(candidate); err != nil {
		fmt.Printf("Error adding ICE candidate: %v\n", err)
	}
}

func (s *SFUServer) handleTrackSubscription(room *Room, p *Participant, trackID string, subscribe bool) {
	if trackVal, exists := room.tracksMap.Load(trackID); exists {
		track := trackVal.(*MediaTrack)
		fmt.Printf("Track subscription: ID=%s, Type=%s, Subscribe=%v\n", trackID, track.Type, subscribe)

		if subscribe {
			track.Subscribers.Store(p.id, p.pc)
			sender, err := p.pc.AddTrack(track.TrackLocal)
			if err != nil {
				fmt.Printf("Error adding track: %v\n", err)
				return
			}
			fmt.Printf("Track added successfully to peer: %s\n", p.id)

			// Запускаем горутину для RTCP
			go func() {
				rtcpBuf := make([]byte, 1500)
				for {
					if _, _, err := sender.Read(rtcpBuf); err != nil {
						return
					}
				}
			}()
		} else {
			track.Subscribers.Delete(p.id)
			fmt.Printf("Unsubscribed peer %s from track %s\n", p.id, trackID)
		}
	} else {
		fmt.Printf("Track not found: %s\n", trackID)
	}
}

func (s *SFUServer) sendExistingTracks(p *Participant, room *Room) {
	var existingTracks []map[string]interface{}

	room.tracksMap.Range(func(key, value interface{}) bool {
		trackID := key.(string)
		track := value.(*MediaTrack)

		trackInfo := map[string]interface{}{
			"trackID":   trackID,
			"ownerID":   track.OwnerID,
			"streamID":  track.StreamID,
			"trackType": track.Type,
		}
		existingTracks = append(existingTracks, trackInfo)
		return true
	})

	if len(existingTracks) > 0 {
		p.ws.WriteJSON(WebSocketMessage{
			Event: "existing_tracks",
			Data:  existingTracks,
		})
	}
}
