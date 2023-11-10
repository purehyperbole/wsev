# wsev

An event based websocket server implementation based on epoll, designed for ease of use and high connection concurrency.

Some parts for reading and writing websocket headers have been derrvied from the excellent [github.com/gobwas/ws](https://github.com/gobwas/ws) and lightly modified to support buffer reuse.

## Features

- [x] Epoll based websocket handler
- [x] SO_REUSEPORT for multiple epoll listeners on the same port
- [x] pooled buffer reuse

## Setup

```sh
go get github.com/purehyperbole/wsev
```

## Usage

```go
package main

import (
    "log"
    
    "github.com/purehyperbole/wsev"
)

func main() {
    h := &wsev.Handler{
			OnConnect: func(conn net.Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn net.Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn net.Conn, msg []byte) {
				msgch <- &testevent{conn: conn, msg: msg}
			},
		}
    New(
		,
		WithReadDeadline(time.Millisecond*100),
	).Serve(9000)
}
```



## Development