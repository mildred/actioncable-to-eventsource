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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/potato2003/actioncable-client-go"
	"github.com/segmentio/ksuid"
)

const (
	QueueSize       = 32
	SessionDuration = 5 * time.Minute
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

type HttpError struct {
	Error string `json:"error"`
}

type Client struct {
	CableServer *url.URL
	Handler     *Handler

	ctx           context.Context
	cancel        context.CancelFunc
	id            string
	lastId        int
	es            *Broker
	cable         *actioncable.Consumer
	subscriptions []*Subscription
	authHeaders   http.Header
	events        chan *WsEvent
	cleanCtx      context.Context
	cleanCancel   context.CancelFunc
}

func setRequestOriginIfNotSet(id string, r *http.Request) {
	// Determine Origin header from Referer
	if r.Header.Get("Origin") != "" || r.Header.Get("Referer") == "" {
		if r.Header.Get("Origin") == "" {
			l.Infof("Origin header missing and no Referrer: %+v", r.Header)
		}
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

	l.Debugf("[%v] Set Origin: %s", id, origin.String())

	r.Header.Set("Origin", origin.String())
}

func (h *Handler) NewClient(ctx0 context.Context, cableServer *url.URL, w http.ResponseWriter, r *http.Request, queue int) *Client {
	var err error
	ctx, cancel := context.WithCancel(ctx0)
	c := &Client{
		Handler:     h,
		CableServer: cableServer,
		ctx:         ctx,
		cancel:      cancel,
		es:          NewBroker(),
		id:          r.URL.Path,
		authHeaders: http.Header{},
		events:      nil,
	}

	if queue > 0 {
		c.events = make(chan *WsEvent, queue)
	}

	setRequestOriginIfNotSet(c.id, r)

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
	l.Debugf("[%v] Connect to ActionCable %s (Origin: %v)", c.id, cableServer, headers.Get("Origin"))
	c.cable, err = actioncable.CreateConsumer(cableServer, opt)

	if err != nil {
		l.Infof("Server Error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&HttpError{
			Error: http.StatusText(http.StatusInternalServerError),
		})
		cancel()
		return nil
	}

	l.Debugf("[%v] Connecting to %v", c.id, cableServer)

	err = c.cable.Connect(ctx)
	if err != nil {
		l.Debugf("[%v] Failed to connect: %v", c.id, err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(&HttpError{
			Error: err.Error(),
		})
		return nil
	}

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
			l.Debugf("[%v] Closed last connection to EventSource", r.URL.Path)
			c.cancel()
		} else {
			l.Debugf("[%v] Closed connection to EventSource (still %d connected)", r.URL.Path, c.es.ConsumersCount())
		}
	}()

	setRequestOriginIfNotSet(c.id, r)
	if c.Handler.StrictHeaders {
		for header := range c.authHeaders {
			if r.Header.Get(header) != c.authHeaders.Get(header) {
				l.Infof("[%v] Forbidden: mismatch header %s\n\texpected: %s\n\tactual:   %s", c.id, header, c.authHeaders.Get(header), r.Header.Get(header))
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	go func() {
		<-ctx.Done()
		data, err := json.Marshal(MetaEvent{Type: "joined", Id: c.id})
		if err != nil {
			l.Infof("[%v] Error sending event (ignoring): %v", c.id, err)
		}
		c.es.SendEventMessage(string(data), "message", c.getID())
	}()
	c.es.ServeHTTP(w, r, cancel)
}

type LongPollResponse struct {
	Message     *WsEvent `json:"message"`
	QueueLength int      `json:"queue_length"`
}

func (c *Client) ServeHTTPSingleEvent(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	l.Debugf("[%v] Serve HTTP... (single event)", c.id)

	setRequestOriginIfNotSet(c.id, r)
	if c.Handler.StrictHeaders {
		for header := range c.authHeaders {
			if r.Header.Get(header) != c.authHeaders.Get(header) {
				l.Infof("[%v] Forbidden: mismatch header %s\n\texpected: %s\n\tactual:   %s", c.id, header, c.authHeaders.Get(header), r.Header.Get(header))
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
	}

	var res LongPollResponse

	select {
	case msg := <-c.events:
		l.Infof("[%v] Dequeue long-poll event #%d in client=%p", c.id, len(c.events), c)
		res.Message = msg
	case <-ctx.Done():
		l.Infof("[%v] Timeout long-poll in client=%p (queue=%d)", c.id, c, len(c.events))
		break
	}

	res.QueueLength = len(c.events)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)

	// Start the clean goroutine
	go c.deferCleanup()
}

func (c *Client) deferCleanup() {
	// Stop other clean goroutines if it exists
	if c.cleanCancel != nil {
		c.cleanCancel()
	}

	c.cleanCtx, c.cleanCancel = context.WithCancel(context.Background())
	sessionCtx, sessionCtxCancel := context.WithTimeout(c.ctx, SessionDuration)
	select {
	case <-sessionCtx.Done():
		// Cancel session
		c.cancel()
		c.cleanCancel()
		sessionCtxCancel() // Just to make linter happy, context is dead
	case <-c.cleanCtx.Done():
		// Cleanup was cancelles before it could cancel the
		// session. Cleanup the context and no nothing else
		sessionCtxCancel()
	}
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
	l.Debugf("[%v#%v] Send %s %s: %+v", ch.client.id, ch.id, eventType, id, event)
	if event.RawMessage != nil && len(*event.RawMessage) > 0 {
		event.ReadJSON(&event.Message)
	}
	ev := WsEvent{ChanId: ch.id, Identifier: ch.channel, Type: event.Type, Message: event.Message}
	if ch.client.events != nil {
		ch.client.events <- &ev
		l.Infof("[%v] Post long-poll event #%d for %v in client=%p", ch.client.id, len(ch.client.events), ch.id, ch.client)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		l.Infof("[%v#%v] Error sending event (ignoring): %v", ch.client.id, ch.id, err)
	}
	ch.client.es.SendEventMessage(string(data), eventType, id)
}

func (ch *ChanHandler) OnConnected(event *actioncable.SubscriptionEvent) {
	l.Debugf("[%v#%v] connected: %+v", ch.client.id, ch.id, event)
	ch.sendEvent(event)
}

func (ch *ChanHandler) OnDisconnected(event *actioncable.SubscriptionEvent) {
	l.Debugf("[%v#%v] disconnected: %+v", ch.client.id, ch.id, event)
	ch.sendEvent(event)
}

func (ch *ChanHandler) OnRejected(event *actioncable.SubscriptionEvent) {
	l.Debugf("[%v#%v] rejected: %+v", ch.client.id, ch.id, event)
	ch.sendEvent(event)
}

func (ch *ChanHandler) OnReceived(event *actioncable.SubscriptionEvent) {
	l.Debugf("[%v#%v] received: %+v", ch.client.id, ch.id, event)
	ch.sendEvent(event)
}

func (c *Client) Subscribe(channel *actioncable.ChannelIdentifier) (string, error) {
	id := ksuid.New().String()
	l.Debugf("[%v#%v] Subscribe: %+v", c.id, id, channel)
	subsc, err := c.cable.Subscriptions.Create(channel)
	subsc.SetHandler(&ChanHandler{c, channel, id})
	c.subscriptions = append(c.subscriptions, &Subscription{subsc, id})
	return id, err
}

func (c *Client) Unsubscribe(channel *actioncable.ChannelIdentifier) (string, error) {
	for i, subsc := range c.subscriptions {
		if subsc.Identifier.Equals(channel) {
			l.Debugf("[%v#%v] Unsubscribe: %+v", c.id, subsc.Id, channel)
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
			l.Debugf("[%v#%v] Received message: %+v", c.id, subsc.Id, message)
			subsc.Send(message)
			return subsc.Id, nil
		}
	}
	return "", nil
}

type Handler struct {
	lock          sync.Mutex
	CableServer   *url.URL
	AllowReuse    bool
	StrictHeaders bool

	clients map[string]*Client
}

func (h *Handler) GetClient(request_uri string) (*Client, bool) {
	h.lock.Lock()
	defer h.lock.Unlock()

	client, ok := h.clients[request_uri]
	return client, ok
}

func (h *Handler) GetClientOrCreate(request_uri string, ctx0 context.Context, cableServer *url.URL, w http.ResponseWriter, r *http.Request, queue int) (*Client, bool) {
	h.lock.Lock()
	defer h.lock.Unlock()

	client, ok := h.clients[request_uri]

	if !ok {
		client = h.NewClient(ctx0, cableServer, w, r, queue)
		if client != nil {
			h.setClientUnsafe(request_uri, client)
		}
	}

	return client, ok
}

func (h *Handler) SetClient(request_uri string, client *Client) {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.setClientUnsafe(request_uri, client)
}

func (h *Handler) setClientUnsafe(request_uri string, client *Client) {
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

	accept := r.Header.Get("Accept")

	pollQueueStr := r.URL.Query().Get("poll-queue")
	pollQueue, _ := strconv.ParseInt(pollQueueStr, 10, strconv.IntSize)
	if pollQueue == 0 {
		pollQueue = QueueSize
	}

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
	} else if r.Method == http.MethodGet && strings.HasPrefix(accept, "text/event-stream") {
		client, ok := h.GetClient(r.URL.Path)
		if ok {
			if h.AllowReuse {
				l.Debugf("[%v] Join existing EventSource", r.URL.Path)
				client.ServeHTTP(w, r)
				// TODO: let the EventSource extend past the original connection context
			} else {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, "Access to EventSource forbidden (not allowing reuse)")
				// Not that good but the client knows the EventSource is attached to someone else and it can send requests to it.
				// Perhaps we should close the EventSource to prevent that.
			}
		} else {
			l.Debugf("[%v] Creating EventSource", r.URL.Path)
			var ctx context.Context
			if h.AllowReuse {
				ctx = context.Background()
			} else {
				ctx = r.Context()
			}
			client := h.NewClient(ctx, h.CableServer, w, r, 0)
			if client != nil {
				h.SetClient(r.URL.Path, client)
				client.ServeHTTP(w, r)
			}
		}
	} else if r.Method == http.MethodGet {
		ctx := r.Context()

		timeoutStr := r.URL.Query().Get("timeout")
		timeout, _ := strconv.ParseInt(timeoutStr, 10, strconv.IntSize)
		if timeout >= 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
			defer cancel()
		}

		client, ok := h.GetClientOrCreate(r.URL.Path, context.Background(), h.CableServer, w, r, int(pollQueue))
		if ok {
			l.Debugf("[%v] Join existing EventSource", r.URL.Path)
			client.ServeHTTPSingleEvent(ctx, w, r)
		} else {
			l.Debugf("[%v] Creating EventSource", r.URL.Path)
			// Start client in background, it is cancelled by
			// client.ServeHTTPSingleEvent below
			if client != nil {
				client.ServeHTTPSingleEvent(ctx, w, r)
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

		client, ok := h.clients[r.URL.Path]
		if !ok || client == nil {
			if pollQueueStr != "" {
				l.Debugf("[%v] Creating EventSource", r.URL.Path)
				client = h.NewClient(context.Background(), h.CableServer, w, r, int(pollQueue))
				h.SetClient(r.URL.Path, client)
				go client.deferCleanup()
			} else {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, "EventSource channel does not exists: %v", r.URL.Path)
			}
		}

		var responses []Response

		for _, req := range requests {
			var id string
			l.Debugf("[%v] Request: %+v", r.URL.Path, req)
			identifier, err := GetChanIdentifier(req.Identifier)
			l.Debugf("[%v] client: %+v", r.URL.Path, client)
			l.Debugf("[%v] identifier: %+v", r.URL.Path, identifier)
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
	flag.BoolVar(&handler.StrictHeaders, "strict-headers", false, "Enforce same headers between requests")
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
