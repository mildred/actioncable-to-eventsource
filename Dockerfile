FROM golang:1.16 AS build

RUN mkdir /src /app
WORKDIR /src
COPY . /src
RUN CGO_ENABLED=0 go build -o /app/actioncable-to-eventsource .

FROM alpine
COPY --from=build /app /app
WORKDIR /app
RUN ln -s /app/actioncable-to-eventsource /bin
CMD actioncable-to-eventsource
