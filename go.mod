module github.com/mildred/actioncable-to-eventsource

go 1.16

require (
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/jpillora/backoff v1.0.0 // indirect
	github.com/potato2003/actioncable-client-go v0.0.0-20200530121345-f064d751d145
	github.com/segmentio/ksuid v1.0.4
	github.com/smartystreets/goconvey v1.7.2 // indirect
)

replace github.com/potato2003/actioncable-client-go => github.com/mildred/actioncable-client-go v0.0.0-20220502140028-877adb02fdfc
