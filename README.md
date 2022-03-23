actioncable-to-eventsource
==========================

This small server written in Go will convert an ActionCable websocket to the
EventSource API, allowing wb clients to communicate with ActionCable even though
in some circumstances WebSockets is not available (corporate environment,
proxy, ...)

HTTP API
--------

A web client first need to generate a unique and secure identifier and generate
a communication URL with it. For example, if the EventSource server is listening
on `http://localhost:8080` (the default) it should Generate an URL of the form
`http://localhost:8080/{generated-id}`.

Note that CORS headers are automatically set and any cookie passed will be
forwarded to the WebSocket server.

- GET `http://{host}/{events-id}`: Starts an EventSource session. The `message`
  event will be passed all the messages of the session.

  A command-line setting can allow multiple listeners to the same EventSource
  session.

  Each `message` event consist of a JSON object of one of the following types:

      - Join message, indicates that the EventSource channel received a new
        listener

            {
                "type": "joined",
                "id": the EventSource session identifier (URL path)
            }

      - ActionCable message

            {
                "type":       "connected" | "disconnected" | "rejected" | "received",
                "chan_id":    a unique identifier for the ActionCable channel (maps to the identifier)
                "identifier": the ActionCable identifier {"channel": "", ...}
                "message":    {...}
            }

- POST `http://{host}/{events-id}`: Manipulates the EventSource session with
  requests, allowing to connect to ActionCable channels. The request body must
  be a JSON array containing one or more requests. A request is a JSON object in
  the form of:

      - Subscribe requests: subscribe to an ActionCable channel

            {
              "identifier": {"channel": "...", ...},
              "subscribe":  true
            }

      - Unsubscribe requests: unsubscribe to an ActionCable channel

            {
              "identifier":  {"channel": "...", ...},
              "unsubscribe": true
            }

      - Send requests: send a message to an ActionCable channel

            {
              "identifier": {"channel": "...", ...},
              "send":       {...}
            }

    Note that the actual request body is an array of these requests, for
    example:

        [
          {
            "identifier": {"channel": "chat", "user_id": 123},
            "subscribe":  true
          },
          {
            "identifier": {"channel": "chat_presence", "user_id": 123},
            "subscribe":  true
          },
        ]

    The response is an array of responses mapping to each request, each
    individual response is of the form:

        {
          "error":   optional error,
          "chan_id": a unique identifier for the ActionCable channel (maps to the identifier) 
        }

Deployment
----------

Use HTTPS behind a reverse proxy, and if possible serve it on the same hostname
than the original application.
