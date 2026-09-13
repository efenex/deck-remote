package main

import "sync"

type codexPendingRequest struct {
	RequestID string
	Text      string
	Baseline  int64
	TurnID    string
	UserSeq   int64
}

type codexRequestTracker struct {
	mu sync.Mutex
	m  map[string][]codexPendingRequest
}

func newCodexRequestTracker() *codexRequestTracker {
	return &codexRequestTracker{m: map[string][]codexPendingRequest{}}
}

func (t *codexRequestTracker) add(sessionID string, req codexPendingRequest) {
	t.mu.Lock()
	t.m[sessionID] = append(t.m[sessionID], req)
	t.mu.Unlock()
}

type correlatedReply struct {
	RequestID string
	Message   histMsg
}

// reconcile binds a delivered request to the first matching rollout user event
// after its baseline, then to a final answer from that same task boundary.
func (t *codexRequestTracker) reconcile(sessionID string, snap harnessSnapshot) []correlatedReply {
	t.mu.Lock()
	defer t.mu.Unlock()
	pending := t.m[sessionID]
	if len(pending) == 0 {
		return nil
	}
	usedUser := map[int64]bool{}
	for _, req := range pending {
		if req.UserSeq > 0 {
			usedUser[req.UserSeq] = true
		}
	}
	for i := range pending {
		if pending[i].TurnID != "" {
			continue
		}
		for _, msg := range snap.Messages {
			if msg.Role == "user" && msg.Seq > pending[i].Baseline && msg.Content == pending[i].Text && !usedUser[msg.Seq] {
				pending[i].TurnID, pending[i].UserSeq = msg.TurnID, msg.Seq
				usedUser[msg.Seq] = true
				break
			}
		}
	}
	var replies []correlatedReply
	keep := pending[:0]
	for _, req := range pending {
		matched := false
		if req.UserSeq > 0 {
			for _, msg := range snap.Messages {
				if msg.Role == "reply" && msg.Seq > req.UserSeq && msg.TurnID == req.TurnID {
					replies = append(replies, correlatedReply{RequestID: req.RequestID, Message: msg})
					matched = true
					break
				}
			}
		}
		if !matched {
			keep = append(keep, req)
		}
	}
	if len(keep) == 0 {
		delete(t.m, sessionID)
	} else {
		t.m[sessionID] = keep
	}
	return replies
}
