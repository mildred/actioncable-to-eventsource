package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
)

type Event struct {
	Event string
	Data  string
}

func (ev *Event) ToString() string {
	event := ev.Event
	if event == "" {
		event = "message"
	}
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, ev.Data)
}

func (ev *Event) WriteTo(w io.Writer) (int64, error) {
	event := ev.Event
	if event == "" {
		event = "message"
	}
	n, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, ev.Data)
	return int64(n), err
}

// Broker accepts subscriptions from clients and publishes messages to them all
type Broker struct {
	subscribers map[chan Event]bool
	sync.Mutex
}

// Subscribe adds a client to the broker
func (b *Broker) Subscribe() chan Event {
	l.Debug("request lock")
	defer l.Debug("release lock")
	b.Lock()
	defer b.Unlock()
	l.Debug("subscribing to broker")
	ch := make(chan Event)
	b.subscribers[ch] = true
	return ch
}

// Unsubscribe removes a client from the broker
func (b *Broker) Unsubscribe(ch chan Event) {
	l.Debug("request lock")
	defer l.Debug("release lock")
	b.Lock()
	defer b.Unlock()
	l.Debug("unsubscribing from broker")
	close(ch)
	delete(b.subscribers, ch)
}

// Publish sends a slice of bytes to all subscribed clients
func (b *Broker) Publish(msg Event) {
	l.Debug("request lock")
	defer l.Debug("release lock")
	b.Lock()
	defer b.Unlock()
	l.Debugf("Publishing to %d subscribers\n", len(b.subscribers))
	for ch := range b.subscribers {
		ch <- msg
	}
}

func (b *Broker) SendEventMessage(data, event, id string) {
	b.Publish(Event{event, data})
}

func (b *Broker) ConsumersCount() int {
	return len(b.subscribers)
}

// NewBroker creates a new broker
func NewBroker() *Broker {
	return &Broker{subscribers: make(map[chan Event]bool)}
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request, cancel context.CancelFunc) {
	ctx := r.Context()
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := b.Subscribe()
	defer b.Unsubscribe(ch)

	if cancel != nil {
		cancel()
	}

	for {
		select {
		case msg := <-ch:
			l.Debug("Send message")
			msg.WriteTo(w)
			f.Flush()
		case <-ctx.Done():
			l.Debug("Done")
			return
		}
	}
}
