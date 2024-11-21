package entities

import (
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"sync"
)

type Participant struct {
	pc                *webrtc.PeerConnection
	ws                *websocket.Conn
	id                string
	mu                sync.RWMutex
	pendingCandidates []webrtc.ICECandidateInit
}
