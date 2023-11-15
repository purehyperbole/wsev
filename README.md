# wsev

An event based websocket server implementation based on epoll, designed for ease of use and high connection concurrency.

Some parts for reading and writing websocket headers have been derrvied from the excellent [github.com/gobwas/ws](https://github.com/gobwas/ws) and lightly modified to support buffer reuse.

## Features

- [x] Epoll based websocket handler
- [x] SO_REUSEPORT for multiple epoll listeners on the same port
- [x] Pooled write buffers for efficient memory usage
- [x] Passes the [autobahn testsuite](https://github.com/crossbario/autobahn-testsuite)
- [ ] Detect connection timeouts

## Setup

```sh
go get github.com/purehyperbole/wsev
```

## Usage

```go
package main

import (
    "log"
    "runtime"

    "github.com/purehyperbole/wsev"
)

func main() {
    h := &wsev.Handler{
        OnConnect: func(conn *wsev.Conn) {
            // client has connected
        },
        OnDisconnect: func(conn *wsev.Conn, err error) {
            // client has disconnected
        },
        OnPing: func(conn *wsev.Conn) {
            // client has sent pong
        },
        OnMessage: func(conn *wsev.Conn, msg []byte) {
            // client has sent a binary/text event
        },
        OnError: func(err error, isFatal bool) {
            // server has experienced an error
        }
    }

    // will start a new websocket server on port 9000
    // an event listener will be started for each cpu
    // as determined by GOMAXPROCS
    err := wsev.New(
        h, 
        // 
        // the deadline that will be set when reading from sockets that have data
        wsev.WithReadDeadline(time.Millisecond*100),
        // the deadline that data will be flushed to the underlying connection 
        // when the data in the buffer has not exceeded the buffer size
        wsev.WithWriteBufferDeadline(time.Millisecond * 100),
        // the size of the write buffer for a connection. these buffers are 
        // only allocated and used when there is data ready for writing to
        // the connection. Once the buffer has been flushed, the buffer is
        // returned to a pool for reuse. 
        wsev.WithWriteBufferSize(1<<14),
        // sets the size of the read buffer for reading from the connection.
        // a read buffer is allocated per event loop
        wsev.WithReadBufferSize(1<<14),
    ).Serve(9000)

    if err != nil {
        log.Fatal(err)
    }

    runtime.Goexit()
}
```

## Example - Chat Room

```go
package main

import (
    "io"
    "log"
    "net"
    "runtime"
    "sync"

    "github.com/purehyperbole/wsev"
)

func main() {
    // keep a list of all connected members
    var connections []io.Writer
    var lock sync.Mutex

    h := &wsev.Handler{
        OnConnect: func(conn *wsev.Conn) {
            // client has connected, so add it to the list
            lock.Lock()
            defer lock.Unlock()

            connections = append(connections, conn)
        },
        OnDisconnect: func(conn *wsev.Conn, err error) {
            // client has disconnected, so remove it from the list
            lock.Lock()
            defer lock.Unlock()

            for i := len(connections) - 1; i >= 0; i-- {
                if connections[i] == conn {
                    connections = append(connections[:i], connections[i+1:]...)
                }
            }
        },
        OnMessage: func(conn *wsev.Conn, msg []byte) {
            // a message has been recevied, broadcast it to the other connections
            lock.Lock()
            defer lock.Unlock()

            _, err := io.MultiWriter(connections...).Write(msg)
            if err != nil {
            	log.Printf("failed to write to connections: %s", err.Error())
            }
        },
    }

    err := wsev.New(
        h, 
    ).Serve(8000)

    if err != nil {
        log.Fatal(err)
    }

    runtime.Goexit()
}
```

## Development

Running tests:

```go
go test -v -race
```

Running with autobahn testsuite (requires [Docker](https://www.docker.com/))

```go
AUTOBAHN=1 go test -v -race
```