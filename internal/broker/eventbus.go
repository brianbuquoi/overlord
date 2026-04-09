package broker

import (
	"sync"
	"time"
)

// TaskEvent is emitted when a task changes state.
type TaskEvent struct {
	Event      string    `json:"event"`
	TaskID     string    `json:"task_id"`
	PipelineID string    `json:"pipeline_id"`
	StageID    string    `json:"stage_id"`
	From       TaskState `json:"from"`
	To         TaskState `json:"to"`
	Timestamp  time.Time `json:"timestamp"`
}

// EventBus is a channel-based pub/sub for task events.
// Subscribers receive events on their own channel. The bus fans out
// each published event to all active subscribers.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[*Subscription]struct{}
}

// Subscription is a single subscriber's handle to the EventBus.
type Subscription struct {
	C      <-chan TaskEvent
	ch     chan TaskEvent
	bus    *EventBus
	closed bool
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[*Subscription]struct{}),
	}
}

// Subscribe returns a new Subscription. The caller must call Unsubscribe
// when done to prevent goroutine/channel leaks.
func (eb *EventBus) Subscribe(bufSize int) *Subscription {
	if bufSize < 1 {
		bufSize = 64
	}
	ch := make(chan TaskEvent, bufSize)
	sub := &Subscription{
		C:   ch,
		ch:  ch,
		bus: eb,
	}
	eb.mu.Lock()
	eb.subscribers[sub] = struct{}{}
	eb.mu.Unlock()
	return sub
}

// Publish sends an event to all subscribers. Non-blocking: if a subscriber's
// buffer is full the event is dropped for that subscriber.
func (eb *EventBus) Publish(event TaskEvent) {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	for sub := range eb.subscribers {
		select {
		case sub.ch <- event:
		default:
			// Subscriber too slow — drop event to avoid blocking the broker.
		}
	}
}

// Unsubscribe removes this subscription from the bus and closes the channel.
func (s *Subscription) Unsubscribe() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	delete(s.bus.subscribers, s)
	close(s.ch)
}
