package biz

import (
	"container/heap"
	"time"
)

type sessionBinding struct {
	sessionHash string
	accountID   string
	expiresAt   time.Time
	heapIndex   int
}

// sessionExpiryHeap keeps one mutable expiry entry per binding. Renewals update
// that entry rather than retaining stale entries for every turn.
type sessionExpiryHeap []*sessionBinding

func (h sessionExpiryHeap) Len() int { return len(h) }

func (h sessionExpiryHeap) Less(i, j int) bool {
	return h[i].expiresAt.Before(h[j].expiresAt)
}

func (h sessionExpiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIndex = i
	h[j].heapIndex = j
}

func (h *sessionExpiryHeap) Push(value any) {
	binding := value.(*sessionBinding)
	binding.heapIndex = len(*h)
	*h = append(*h, binding)
}

func (h *sessionExpiryHeap) Pop() any {
	last := len(*h) - 1
	binding := (*h)[last]
	(*h)[last] = nil
	*h = (*h)[:last]
	binding.heapIndex = -1
	return binding
}

// All binding, count and expiry mutations share scheduleMu with admission and
// release. Callers must hold it (or exclusively own the Account in a test).
func (s *Account) bindSessionLocked(sessionHash, accountID string, expiresAt time.Time) {
	if binding, found := s.sessions[sessionHash]; found {
		if binding.accountID != accountID {
			s.decrementSessionCountLocked(binding.accountID)
			s.sessionCounts[accountID]++
			binding.accountID = accountID
		}
		binding.expiresAt = expiresAt
		heap.Fix(&s.sessionExpiry, binding.heapIndex)
		return
	}
	binding := &sessionBinding{sessionHash: sessionHash, accountID: accountID, expiresAt: expiresAt}
	s.sessions[sessionHash] = binding
	s.sessionCounts[accountID]++
	heap.Push(&s.sessionExpiry, binding)
}

func (s *Account) expireSessionsLocked(now time.Time) {
	for len(s.sessionExpiry) > 0 && !s.sessionExpiry[0].expiresAt.After(now) {
		binding := heap.Pop(&s.sessionExpiry).(*sessionBinding)
		delete(s.sessions, binding.sessionHash)
		s.decrementSessionCountLocked(binding.accountID)
	}
}

func (s *Account) decrementSessionCountLocked(accountID string) {
	if s.sessionCounts[accountID] <= 1 {
		delete(s.sessionCounts, accountID)
	} else {
		s.sessionCounts[accountID]--
	}
}
