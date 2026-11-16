package caseta

import "sync"

// watcher is what one Watch has yet to tell its handler. The connection's
// reading goroutine puts events in, and never waits; the watching
// goroutine takes them out.
type watcher struct {
	wake chan struct{}

	mu     sync.Mutex
	queue  []queued
	behind bool
	// latest is where each zone's status waits in the queue, by the
	// zone's reference.
	latest map[string]int
}

// queued is an event that waits, and for a zone's status the place the
// bridge sent it at.
type queued struct {
	event    Event
	received uint64
}

// zone queues what a zone is at. One status a zone waits: of two, the one
// the bridge sent later stays, whichever reached this goroutine first.
func (w *watcher) zone(status ZoneStatus, received uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if index, waiting := w.latest[status.Zone.Href]; waiting {
		if received >= w.queue[index].received {
			w.queue[index] = queued{event: status, received: received}
		}

		return
	}

	w.latest[status.Zone.Href] = len(w.queue)
	w.put(queued{event: status, received: received})
}

// occurred queues something that happened, which is told however many
// came before it, until the watcher is too far behind.
func (w *watcher) occurred(event Event) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.queue) >= maxBehind {
		w.behind = true
		w.signal()

		return
	}

	w.put(queued{event: event})
}

// put appends and wakes the watching goroutine; the lock is held.
func (w *watcher) put(one queued) {
	w.queue = append(w.queue, one)
	w.signal()
}

func (w *watcher) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// take is everything that waits, in order, and whether the watcher fell
// behind.
func (w *watcher) take() ([]Event, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	events := make([]Event, 0, len(w.queue))
	for _, one := range w.queue {
		events = append(events, one.event)
	}

	w.queue = w.queue[:0]
	clear(w.latest)

	return events, w.behind
}
