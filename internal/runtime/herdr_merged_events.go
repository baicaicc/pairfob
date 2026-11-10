package runtime

import (
	"context"
	"sync"
)

// SubscribeEvents holds one Herdr subscription per running session, plus any
// session the requested panes still name, and delivers their events as one
// stream with merged-view pane ids. Each session also delivers its structural
// events, so a pane appearing in any session reaches the caller. If one
// session's stream ends, the merged stream ends too, and the caller's usual
// resubscribe picks up the sessions running at that point.
func (m *Merged) SubscribeEvents(ctx context.Context, session SessionRef, subscription EventSubscription) (EventStream, error) {
	if err := refuseNamedSession("events.subscribe", session); err != nil {
		return nil, err
	}
	panes := map[string][]string{}
	for _, id := range subscription.PaneIDs {
		target, bare, err := splitMergedID("events.subscribe", id)
		if err != nil {
			return nil, err
		}
		panes[target.Name] = append(panes[target.Name], bare)
	}
	sessions := m.running(ctx)
	listed := map[string]bool{}
	for _, target := range sessions {
		listed[target.Name] = true
	}
	for name := range panes {
		if !listed[name] {
			sessions = append(sessions, NamedSession(name))
			listed[name] = true
		}
	}

	parts := make([]mergedEventPart, 0, len(sessions))
	for _, target := range sessions {
		stream, err := m.herdr.SubscribeEvents(ctx, target, EventSubscription{PaneIDs: panes[target.Name]})
		if err != nil {
			for _, part := range parts {
				_ = part.stream.Close()
			}
			return nil, err
		}
		parts = append(parts, mergedEventPart{session: target, stream: stream})
	}
	return newMergedEventStream(parts), nil
}

type mergedEventPart struct {
	session SessionRef
	stream  EventStream
}

type mergedEventStream struct {
	parts  []mergedEventPart
	events chan Event
	done   chan struct{}
	stop   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
	closed bool
}

func newMergedEventStream(parts []mergedEventPart) *mergedEventStream {
	s := &mergedEventStream{parts: parts, events: make(chan Event, 64), done: make(chan struct{}), stop: make(chan struct{})}
	var wg sync.WaitGroup
	for _, part := range parts {
		wg.Add(1)
		go func(part mergedEventPart) {
			defer wg.Done()
			s.forward(part)
		}(part)
	}
	go func() {
		wg.Wait()
		close(s.events)
		close(s.done)
	}()
	return s
}

func (s *mergedEventStream) forward(part mergedEventPart) {
	for {
		select {
		case <-s.stop:
			return
		case event, ok := <-part.stream.Events():
			if !ok {
				s.end(part.stream.Err())
				return
			}
			event.PaneID = qualifyMergedID(part.session, event.PaneID)
			select {
			case s.events <- event:
			case <-s.stop:
				return
			}
		}
	}
}

// end records why the first session stream finished, unless the caller had
// already closed the merged stream, and stops the others.
func (s *mergedEventStream) end(err error) {
	s.mu.Lock()
	if !s.closed && s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.shutdown()
}

func (s *mergedEventStream) shutdown() {
	s.once.Do(func() {
		close(s.stop)
		for _, part := range s.parts {
			_ = part.stream.Close()
		}
	})
}

func (s *mergedEventStream) Events() <-chan Event  { return s.events }
func (s *mergedEventStream) Done() <-chan struct{} { return s.done }

func (s *mergedEventStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *mergedEventStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.shutdown()
	return nil
}
