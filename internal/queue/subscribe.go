package queue

import "sync"

const subscriberBuffer = 256

type subscribers struct {
	mu   sync.RWMutex
	next int
	subs map[int]chan Event
}

// Subscribe returns a channel of events and a function that ends the subscription (safe to call
// more than once; it closes the channel). A subscriber that does not keep up loses events instead of
// slowing the queue down.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	s := &m.subs
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs == nil {
		s.subs = map[int]chan Event{}
	}
	id := s.next
	s.next++
	ch := make(chan Event, subscriberBuffer)
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
	}
}

func (m *Manager) publish(ev Event) {
	if m.obs != nil && ev.Type != EventOutput {
		m.obsMu.Lock()
		if !m.obsClosed {
			select {
			case m.obs <- ev:
			default:
				m.log.Warn("the observer is too far behind: an event was dropped", "type", ev.Type, "job", ev.Job.ID)
			}
		}
		m.obsMu.Unlock()
	}
	s := &m.subs
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
