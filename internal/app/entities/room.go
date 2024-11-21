package entities

import (
	"runtime"
	"sync"
)

type Room struct {
	ID                string
	participantShards []*participantShard
	tracksMap         sync.Map
}

type participantShard struct {
	participants map[string]*Participant
	mu           sync.RWMutex
}

func NewRoom(id string) *Room {
	shards := make([]*participantShard, runtime.NumCPU())
	for i := range shards {
		shards[i] = &participantShard{
			participants: make(map[string]*Participant),
		}
	}

	return &Room{
		ID:                id,
		participantShards: shards,
	}
}

func (r *Room) getParticipantShard(id string) *participantShard {
	shard := int(id[0]) % len(r.participantShards)
	return r.participantShards[shard]
}
