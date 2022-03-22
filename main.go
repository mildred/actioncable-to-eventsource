package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/potato2003/actioncable-client-go"
	//"gopkg.in/antage/eventsource.v1"
)

var logDebug = log.Default()

type Client struct {
	CableServer *url.URL

	id            string
	lastId        int
	es            *Broker // eventsource.EventSource
	cable         *actioncable.Consumer
	subscriptions []*actioncable.Subscription
}

func NewClient(cableServer *url.URL, w http.ResponseWriter, r *http.Request) *Client {
	var err error
	c := &Client{
		CableServer: cableServer,
		/*
			es: eventsource.New(&eventsource.Settings{
				Timeout:        5 * time.Second,
				CloseOnTimeout: false,
				IdleTimeout:    30 * time.Minute,
			}, func(req *http.Request) [][]byte {
				return [][]byte{
					//[]byte("Connection: close"),
					[]byte("Transfer-Encoding: chunked"),
					[]byte("Cache-Control: no-cache"),
					[]byte("Content-Type: text/event-stream"),
					[]byte("Access-Control-Allow-Methods: GET, POST"),
					[]byte("Access-Control-Allow-Headers: Content-Type, Cookie"),
					[]byte("Access-Control-Allow-Origin: *"),
					[]byte("Access-Control-Allow-Credentials: true"),
				}
			}),
		*/
		es: NewBroker(),
		id: r.RequestURI,
	}

	opt := actioncable.NewConsumerOptions()
	header := &http.Header{}
	for _, head := range []string{"Cookie", "Origin"} {
		for _, val := range r.Header.Values(head) {
			header.Add(head, val)
		}
	}
	opt.SetHeader(header)
	c.cable, err = actioncable.CreateConsumer(cableServer, opt)

	if err != nil {
		log.Printf("Server Error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	}

	logDebug.Printf("[%v] Connecting to %v", c.id, cableServer)

	c.cable.Connect() // FIXME: accept a context

	logDebug.Printf("[%v] Connected to %v", c.id, cableServer)

	return c
}

func (c *Client) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logDebug.Printf("[%v] Serve HTTP...", c.id)
	c.es.ServeHTTP(w, r)
	c.es.SendEventMessage("joined", "meta", c.getID())
}

func (c *Client) getID() string {
	c.lastId = c.lastId + 1
	return fmt.Sprintf("%d", c.lastId)
}

type ChanHandler struct {
	client  *Client
	channel *actioncable.ChannelIdentifier
}

type WsEvent struct {
	Channel *actioncable.ChannelIdentifier `json:"channel"`
	Event   *actioncable.SubscriptionEvent `json:"event"`
}

func (ch *ChanHandler) sendEvent(event *actioncable.SubscriptionEvent) {
	id := ch.client.getID()
	eventType := "message"
	logDebug.Printf("[%v] Send %s %s: %+v", ch.client.id, eventType, id, event)
	data, err := json.Marshal(WsEvent{ch.channel, event})
	if err != nil {
		log.Printf("Error sending event (ignoring): %v", err)
	}
	ch.client.es.SendEventMessage(string(data), eventType, id)
}

func (ch *ChanHandler) OnConnected(event *actioncable.SubscriptionEvent) {
	ch.sendEvent(event)
}

func (ch *ChanHandler) OnDisconnected(event *actioncable.SubscriptionEvent) {
	ch.sendEvent(event)
}

func (ch *ChanHandler) OnRejected(event *actioncable.SubscriptionEvent) {
	ch.sendEvent(event)
}

func (ch *ChanHandler) OnReceived(event *actioncable.SubscriptionEvent) {
	ch.sendEvent(event)
}

func (c *Client) Subscribe(channel *actioncable.ChannelIdentifier) error {
	logDebug.Printf("[%v] Subscribe: %+v", c.id, channel)
	subsc, err := c.cable.Subscriptions.Create(channel)
	subsc.SetHandler(&ChanHandler{c, channel})
	c.subscriptions = append(c.subscriptions, subsc)
	return err
}

func (c *Client) Unsubscribe(channel *actioncable.ChannelIdentifier) error {
	for i, subsc := range c.subscriptions {
		if subsc.Identifier.Equals(channel) {
			logDebug.Printf("[%v] Unsubscribe: %+v", c.id, channel)
			c.subscriptions = append(c.subscriptions[:i], c.subscriptions[i+1:]...)
			subsc.Unsubscribe()
		}
	}
	return nil
}

func (c *Client) Send(channel *actioncable.ChannelIdentifier, message map[string]interface{}) error {
	for _, subsc := range c.subscriptions {
		if subsc.Identifier.Equals(channel) {
			logDebug.Printf("[%v] Received message: %+v", c.id, message)
			subsc.Send(message)
			return nil
		}
	}
	return nil
}

/*
func (c *Client) Close() {
	//for subsc := range c.subscriptions {
	//	subsc.subscription.Unsubscribe()
	//}
	logDebug.Printf("[%v] Request to close EventSource, clients connected: %d", c.id, c.es.ConsumersCount())
	if c.es.ConsumersCount() == 0 {
		c.es.Close()
		logDebug.Printf("[%v] Close EventSource", c.id)
		c.cable.Disconnect()
		c.subscriptions = nil
		c.cable = nil
		c.es = nil
	}
}
*/

type Handler struct {
	CableServer *url.URL
	AllowReuse  bool

	clients map[string]*Client
}

func NewHandler() *Handler {
	return &Handler{
		clients: map[string]*Client{},
	}
}

type Request struct {
	Channel     *actioncable.ChannelIdentifier `json:"channel"`
	Subscribe   bool                           `json:"subscribe"`
	Unsubscribe bool                           `json:"unsubscribe"`
	Send        map[string]interface{}         `json:"send"`
}

type Response struct {
	Error error `json:"error"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		client, ok := h.clients[r.RequestURI]
		if ok {
			if h.AllowReuse {
				logDebug.Printf("[%v] Join existing EventSource", r.RequestURI)
				client.ServeHTTP(w, r)
				// TODO: let the EventSource extend past the original connection context
			} else {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, "Access to EventSource forbidden")
				// Not that good but the client knows the EventSource is attached to someone else and it can send requests to it.
				// Perhaps we should close the EventSource to prevent that.
			}
		} else {
			logDebug.Printf("[%v] Creating EventSource", r.RequestURI)
			client := NewClient(h.CableServer, w, r)
			if client != nil {
				h.clients[r.RequestURI] = client
				client.ServeHTTP(w, r)
				// go func() {
				//logDebug.Printf("[%v] request %+v", r.RequestURI, r)
				//logDebug.Printf("[%v] context %+v", r.RequestURI, r.Context())
				//logDebug.Printf("[%v] Wait for connection to close...", r.RequestURI)
				//<-r.Context().Done() // TODO: find out why the context never cancels
				// Implementation suggests that when r.ctx is nil, it returns a background context...
				logDebug.Printf("[%v] Closed connection to EventSource", r.RequestURI)
				//client.Close()
				if client.es.ConsumersCount() == 0 {
					delete(h.clients, r.RequestURI)
				}
				//}()
			}
		}
	} else if r.Method == http.MethodPost {
		var requests []Request

		dec := json.NewDecoder(r.Body)
		err := dec.Decode(&requests)

		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "Error decoding request: %v", err)
		}

		client, ok := h.clients[r.RequestURI]
		if !ok || client == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "EventSource channel does not exists: %v", r.RequestURI)
		}

		var responses []Response

		for _, req := range requests {
			logDebug.Printf("[%v] Request: %+v", r.RequestURI, req)
			if req.Subscribe {
				err := client.Subscribe(req.Channel)
				responses = append(responses, Response{Error: err})
			} else if req.Unsubscribe {
				err := client.Unsubscribe(req.Channel)
				responses = append(responses, Response{Error: err})
			} else if req.Send != nil {
				err := client.Send(req.Channel, req.Send)
				responses = append(responses, Response{Error: err})
			} else {
				responses = append(responses, Response{Error: fmt.Errorf("Unknown request")})
			}
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		err = enc.Encode(responses)

		if err != nil {
			log.Printf("Error responding to request, cannot marshal in JSON (ignored): %v", err)
		}
	} else {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Incorrect request method %s", r.Method)
	}
}

func serve(ctx context.Context) error {
	var server http.Server
	var cable_url string
	var err error
	handler := NewHandler()

	flag.StringVar(&server.Addr, "l", ":8080", "Listen address and port")
	flag.BoolVar(&handler.AllowReuse, "r", false, "Allow reusing EventSource")
	flag.StringVar(&cable_url, "s", "ws://127.0.0.1:3000/cable", "ActionCable URL")
	flag.Parse()

	handler.CableServer, err = url.Parse(cable_url)
	if err != nil {
		return fmt.Errorf("failed to parse ActionCable server URL, %v", err)
	}

	server.Handler = handler
	server.BaseContext = func(net.Listener) context.Context { return ctx }

	go func() {
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Printf("listen: %+s\n", err)
		}
	}()

	log.Printf("Starting server on %v", server.Addr)
	<-ctx.Done()
	log.Printf("Stopping server")

	// Stoping server

	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		cancel()
	}()

	err = server.Shutdown(ctxShutDown)
	if err != nil && err != http.ErrServerClosed {
		return err
	}

	log.Printf("Server stopped")
	return nil
}

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		oscall := <-c
		log.Printf("system call: %+v", oscall)
		signal.Stop(c)
		cancel()
	}()

	if err := serve(ctx); err != nil {
		log.Printf("failed to serve: +%v\n", err)
	}
}
