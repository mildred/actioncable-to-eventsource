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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/potato2003/actioncable-client-go"
	"github.com/segmentio/ksuid"
)

// export GOFLAGS="-ldflags=-X=main.version=$(git describe --always HEAD)"
// https://goreleaser.com/cookbooks/using-main.version?h=version
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var l *StandardLibLogger = NewStandardLibLogger(log.Default())

type Subscription struct {
	*actioncable.Subscription
	Id string
}

type Client struct {
	CableServer *url.URL

	ctx           context.Context
	cancel        context.CancelFunc
	id            string
	lastId        int
	es            *Broker
	cable         *actioncable.Consumer
	subscriptions []*Subscription
	authHeaders   http.Header
}

func setRequestOriginIfNotSet(r *http.Request) {
	// Determine Origin header from Referer
	if r.Header.Get("Origin") != "" || r.Header.Get("Referer") == "" {
		return
	}

	u, err := url.Parse(r.Header.Get("Referer"))
	if err != nil {
		l.Infof("Cannot parse Referer to construct Origin header: %v", err)
		return
	}

	origin, err := u.Parse("/")
	if err != nil {
		l.Infof("Cannot construct Origin header from Referer: %v", err)
		return
	}

	origin.Path = ""
	origin.RawPath = ""

	l.Debugf("Set Origin: %s", origin.String())

	r.Header.Set("Origin", origin.String())
}

func NewClient(ctx0 context.Context, cableServer *url.URL, w http.ResponseWriter, r *http.Request) *Client {
	var err error
	ctx, cancel := context.WithCancel(ctx0)
	c := &Client{
		CableServer: cableServer,
		ctx:         ctx,
		cancel:      cancel,
		es:          NewBroker(),
		id:          r.RequestURI,
		authHeaders: http.Header{},
	}

	setRequestOriginIfNotSet(r)

	headers := http.Header{}
	opt := actioncable.NewConsumerOptions()
	for _, head := range []string{"Cookie", "Origin"} {
		for _, val := range r.Header.Values(head) {
			c.authHeaders.Add(head, val)
			headers.Add(head, val)
		}
	}
	for header, vals := range r.Header {
		lheader := strings.ToLower(header)
		if strings.HasPrefix(lheader, "x-forwarded-") ||
			lheader == "x-real-ip" {
			headers[header] = vals
		}
	}
	headers.Set("Host", r.Host)
	opt.SetHeader(&headers)
	c.cable, err = actioncable.CreateConsumer(cableServer, opt)

	if err != nil {
		l.Infof("Server Error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		cancel()
		return nil
	}

	l.Debugf("[%v] Connecting to %v", c.id, cableServer)

	c.cable.Connect(ctx)

	l.Debugf("[%v] Connected to %v", c.id, cableServer)

	return c
}

type MetaEvent struct {
	Id   string `json:"id"`
	Type string `json:"type"`
}

func (c *Client) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.Debugf("[%v] Serve HTTP...", c.id)

	defer func() {
		if c.es.ConsumersCount() == 0 {
			l.Debugf("[%v] Closed last connection to EventSource", r.RequestURI)
			c.cancel()
		} else {
			l.Debugf("[%v] Closed connection to EventSource (still %d connected)", r.RequestURI, c.es.ConsumersCount())
		}
	}()

	setRequestOriginIfNotSet(r)
	for header, _ := range c.authHeaders {
		if r.Header.Get(header) != c.authHeaders.Get(header) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	go func() {
		<-ctx.Done()
		data, err := json.Marshal(MetaEvent{Type: "joined", Id: c.id})
		if err != nil {
			l.Infof("Error sending event (ignoring): %v", err)
		}
		c.es.SendEventMessage(string(data), "message", c.getID())
	}()
	c.es.ServeHTTP(w, r, cancel)
}

func (c *Client) getID() string {
	c.lastId = c.lastId + 1
	return fmt.Sprintf("%d", c.lastId)
}

type ChanIdentifier map[string]interface{}

func GetChanIdentifier(id ChanIdentifier) (*actioncable.ChannelIdentifier, error) {
	name, ok := id["channel"].(string)
	if !ok {
		return nil, fmt.Errorf("Incorrect channel identifier, missing \"channel\" key pair in: %+v", id)
	}
	params := make(map[string]interface{})
	for k, v := range id {
		if k != "channel" {
			params[k] = v
		}
	}
	return actioncable.NewChannelIdentifier(name, params), nil
}

type ChanHandler struct {
	client  *Client
	channel *actioncable.ChannelIdentifier
	id      string
}

type WsEvent struct {
	ChanId     string                            `json:"chan_id"`
	Identifier *actioncable.ChannelIdentifier    `json:"identifier"`
	Type       actioncable.SubscriptionEventType `json:"type"`
	Message    interface{}                       `json:"message"`
}

func (ch *ChanHandler) sendEvent(event *actioncable.SubscriptionEvent) {
	id := ch.client.getID()
	eventType := "message"
	l.Debugf("[%v] Send %s %s: %+v", ch.client.id, eventType, id, event)
	if event.RawMessage != nil && len(*event.RawMessage) > 0 {
		event.ReadJSON(&event.Message)
	}
	data, err := json.Marshal(WsEvent{ChanId: ch.id, Identifier: ch.channel, Type: event.Type, Message: event.Message})
	if err != nil {
		l.Infof("Error sending event (ignoring): %v", err)
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

func (c *Client) Subscribe(channel *actioncable.ChannelIdentifier) (string, error) {
	l.Debugf("[%v] Subscribe: %+v", c.id, channel)
	subsc, err := c.cable.Subscriptions.Create(channel)
	id := ksuid.New().String()
	subsc.SetHandler(&ChanHandler{c, channel, id})
	c.subscriptions = append(c.subscriptions, &Subscription{subsc, id})
	return id, err
}

func (c *Client) Unsubscribe(channel *actioncable.ChannelIdentifier) (string, error) {
	for i, subsc := range c.subscriptions {
		if subsc.Identifier.Equals(channel) {
			l.Debugf("[%v] Unsubscribe: %+v", c.id, channel)
			c.subscriptions = append(c.subscriptions[:i], c.subscriptions[i+1:]...)
			subsc.Unsubscribe()
			return subsc.Id, nil
		}
	}
	return "", nil
}

func (c *Client) Send(channel *actioncable.ChannelIdentifier, message map[string]interface{}) (string, error) {
	for _, subsc := range c.subscriptions {
		if subsc.Identifier.Equals(channel) {
			l.Debugf("[%v] Received message: %+v", c.id, message)
			subsc.Send(message)
			return subsc.Id, nil
		}
	}
	return "", nil
}

type Handler struct {
	lock        sync.Mutex
	CableServer *url.URL
	AllowReuse  bool

	clients map[string]*Client
}

func (h *Handler) GetClient(request_uri string) (*Client, bool) {
	h.lock.Lock()
	defer h.lock.Unlock()

	client, ok := h.clients[request_uri]
	if ok && client.ctx.Err() == nil {
		delete(h.clients, request_uri)
	}
	return client, ok
}

func (h *Handler) SetClient(request_uri string, client *Client) {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.clients[request_uri] = client
	go func() {
		<-client.ctx.Done()
		h.lock.Lock()
		defer h.lock.Unlock()
		delete(h.clients, request_uri)
	}()
}

func NewHandler() *Handler {
	return &Handler{
		clients: map[string]*Client{},
	}
}

type Request struct {
	Identifier  ChanIdentifier         `json:"identifier"`
	Subscribe   bool                   `json:"subscribe"`
	Unsubscribe bool                   `json:"unsubscribe"`
	Send        map[string]interface{} `json:"send"`
}

type Response struct {
	ChanId string `json:"chan_id"`
	Error  error  `json:"error"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Cookie, Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
	} else if r.Method == http.MethodGet {
		client, ok := h.GetClient(r.RequestURI)
		if ok {
			if h.AllowReuse {
				l.Debugf("[%v] Join existing EventSource", r.RequestURI)
				client.ServeHTTP(w, r)
				// TODO: let the EventSource extend past the original connection context
			} else {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, "Access to EventSource forbidden")
				// Not that good but the client knows the EventSource is attached to someone else and it can send requests to it.
				// Perhaps we should close the EventSource to prevent that.
			}
		} else {
			l.Debugf("[%v] Creating EventSource", r.RequestURI)
			var ctx context.Context
			if h.AllowReuse {
				ctx = context.Background()
			} else {
				ctx = r.Context()
			}
			client := NewClient(ctx, h.CableServer, w, r)
			if client != nil {
				h.SetClient(r.RequestURI, client)
				client.ServeHTTP(w, r)
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
			var id string
			l.Debugf("[%v] Request: %+v", r.RequestURI, req)
			identifier, err := GetChanIdentifier(req.Identifier)
			if err == nil && identifier != nil && req.Subscribe {
				id, err = client.Subscribe(identifier)
			} else if err == nil && identifier != nil && req.Unsubscribe {
				id, err = client.Unsubscribe(identifier)
			} else if err == nil && identifier != nil && req.Send != nil {
				id, err = client.Send(identifier, req.Send)
			} else if err == nil {
				err = fmt.Errorf("Unknown request")
			}
			responses = append(responses, Response{ChanId: id, Error: err})
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		err = enc.Encode(responses)

		if err != nil {
			l.Infof("Error responding to request, cannot marshal in JSON (ignored): %v", err)
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
	var quiet bool
	handler := NewHandler()

	flag.StringVar(&server.Addr, "l", ":8080", "Listen address and port")
	flag.BoolVar(&handler.AllowReuse, "r", false, "Allow reusing EventSource")
	flag.BoolVar(&l.DebugLevel, "d", false, "Debug log")
	flag.BoolVar(&quiet, "q", false, "Quiet log")
	flag.StringVar(&cable_url, "s", "ws://127.0.0.1:3000/cable", "ActionCable URL")

	versionFlag := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("Version: %s\n", version)
		fmt.Printf("Build: %s at %s\n", commit, date)
		return nil
	}

	l.InfoLevel = !quiet

	handler.CableServer, err = url.Parse(cable_url)
	if err != nil {
		return fmt.Errorf("failed to parse ActionCable server URL, %v", err)
	}

	server.Handler = handler
	server.BaseContext = func(net.Listener) context.Context { return ctx }

	go func() {
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			l.Infof("listen: %+s\n", err)
			os.Exit(1)
		}
	}()

	l.Infof("Starting server on %v", server.Addr)
	<-ctx.Done()
	l.Infof("Stopping server")

	// Stoping server

	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		cancel()
	}()

	err = server.Shutdown(ctxShutDown)
	if err != nil && err != http.ErrServerClosed {
		return err
	}

	l.Infof("Server stopped")
	return nil
}

func main() {
	actioncable.SetLogger(l)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		oscall := <-c
		l.Infof("system call: %+v", oscall)
		signal.Stop(c)
		cancel()
	}()

	if err := serve(ctx); err != nil {
		l.Infof("failed to serve: +%v\n", err)
	}
}
