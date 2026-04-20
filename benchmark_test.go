package wsev

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func benchconn(b *testing.B, address string) (net.Conn, *bufio.Reader) {
	req, err := http.NewRequest("GET", "ws://"+address, nil)
	if err != nil {
		b.Fatal(err)
	}

	d := net.Dialer{
		KeepAlive: time.Duration(-1),
	}

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := d.Dial("tcp", address)
	if err != nil {
		b.Fatal(err)
	}

	err = conn.SetDeadline(time.Time{})
	if err != nil {
		b.Fatal(err)
	}

	err = req.Write(conn)
	if err != nil {
		b.Fatal(err)
	}

	rb := bufio.NewReaderSize(conn, 1<<14)

	resp, err := http.ReadResponse(rb, req)
	if err != nil {
		b.Fatal(err)
	}

	if resp.Header.Get("Connection") != "Upgrade" || resp.Header.Get("Upgrade") != "websocket" {
		b.Fatal("invalid upgrade response")
	}

	resp.Close = false

	return conn, rb
}

func BenchmarkEcho(b *testing.B) {
	payloadSizes := []int{64, 1024, 4096}

	for _, size := range payloadSizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			s := New(&Handler{
				OnBinary: func(conn *Conn, msg []byte) {
					conn.Write(msg)
				},
			}, WithWriteBufferDeadline(time.Microsecond), WithReadDeadline(time.Hour))

			port, address := nexttestport()

			go func() {
				s.Serve(port)
			}()

			time.Sleep(100 * time.Millisecond)

			conn, rb := benchconn(b, address)
			defer conn.Close()
			defer s.Close()

			payload := make([]byte, size)
			rand.Read(payload)

			codec := newCodec()
			buf, err := newCircbuf(4096)
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			b.ReportAllocs()
			b.SetBytes(int64(size))

			for i := 0; i < b.N; i++ {
				mcopy := make([]byte, len(payload))
				copy(mcopy, payload)

				h := header{
					OpCode: opBinary,
					Fin:    true,
					Masked: true,
					Length: int64(len(payload)),
				}

				cipher(mcopy, h.Mask, 0)

				err := codec.WriteHeader(conn, h)
				if err != nil {
					b.Fatal(err)
				}

				_, err = conn.Write(mcopy)
				if err != nil {
					b.Fatal(err)
				}

				var rh header

				n, err := rb.Read(buf.next())
				if err != nil {
					b.Fatal(err)
				}
				buf.advanceWrite(n)

				err = codec.ReadHeader(&rh, buf)
				if err != nil {
					b.Fatal(err)
				}

				if buf.buffered() < int(rh.Length) {
					needed := int(rh.Length) - buf.buffered()

					n, err := io.ReadFull(rb, buf.next()[:needed])
					if err != nil {
						b.Fatal(err)
					}
					buf.advanceWrite(n)
				}

				buf.advanceRead(int(rh.Length))
			}
		})
	}
}

func BenchmarkBroadcast(b *testing.B) {
	payloadSizes := []int{64, 1024}

	for _, size := range payloadSizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			var connections []net.Conn
			var lock sync.Mutex
			var wg sync.WaitGroup
			wg.Add(10)

			s := New(&Handler{
				OnConnect: func(conn *Conn) {
					lock.Lock()
					defer lock.Unlock()

					connections = append(connections, conn)
					wg.Done()
				},
				OnDisconnect: func(conn *Conn, err error) {
					lock.Lock()
					defer lock.Unlock()

					for i := len(connections) - 1; i >= 0; i-- {
						if connections[i] == conn {
							connections = append(connections[:i], connections[i+1:]...)
						}
					}
				},
				OnBinary: func(conn *Conn, msg []byte) {
					lock.Lock()
					defer lock.Unlock()

					for _, c := range connections {
						c.Write(msg)
					}
				},
			}, WithWriteBufferDeadline(time.Microsecond), WithReadDeadline(time.Hour))

			port, address := nexttestport()

			go func() {
				s.Serve(port)
			}()

			time.Sleep(100 * time.Millisecond)

			var clients []net.Conn
			var clientRBs []*bufio.Reader

			for i := 0; i < 10; i++ {
				c, rb := benchconn(b, address)
				clients = append(clients, c)
				clientRBs = append(clientRBs, rb)
			}

			wg.Wait()

			payload := make([]byte, size)
			rand.Read(payload)
			codec := newCodec()

			clientBufs := make([]*circbuf, len(clients))
			for i := range clientBufs {
				clientBufs[i], _ = newCircbuf(4096)
			}

			b.ResetTimer()
			b.ReportAllocs()
			b.SetBytes(int64(size * len(clients)))

			for i := 0; i < b.N; i++ {
				mcopy := make([]byte, len(payload))
				copy(mcopy, payload)

				h := header{
					OpCode: opBinary,
					Fin:    true,
					Masked: true,
					Length: int64(len(payload)),
				}

				cipher(mcopy, h.Mask, 0)

				err := codec.WriteHeader(clients[0], h)
				if err != nil {
					b.Fatal(err)
				}
				_, err = clients[0].Write(mcopy)
				if err != nil {
					b.Fatal(err)
				}

				for j, rb := range clientRBs {
					var rh header
					buf := clientBufs[j]

					n, err := rb.Read(buf.next())
					if err != nil {
						b.Fatal(err)
					}
					buf.advanceWrite(n)

					err = codec.ReadHeader(&rh, buf)
					if err != nil {
						b.Fatal(err)
					}

					if buf.buffered() < int(rh.Length) {
						needed := int(rh.Length) - buf.buffered()
						n, err := io.ReadFull(rb, buf.next()[:needed])
						if err != nil {
							b.Fatal(err)
						}
						buf.advanceWrite(n)
					}

					buf.advanceRead(int(rh.Length))
				}
			}

			for _, c := range clients {
				c.Close()
			}
			s.Close()
		})
	}
}

func BenchmarkEchoPipelined(b *testing.B) {
	// note: 64 byte payloads never really fill the connection buffer, so
	// only flushes to connection when the flush timer runs so expect to see
	// much worse performance for the smallest test parameters. increasing
	// batch size here will ensure the buffer is filled and proactively flushed.
	payloadSizes := []int{64, 1024, 4096}
	bufferSizes := []int{4096, 16384, 65536}
	batchSize := 100

	for _, bufSize := range bufferSizes {
		for _, size := range payloadSizes {
			b.Run(fmt.Sprintf("BufSize_%d_Payload_%d", bufSize, size), func(b *testing.B) {
				s := New(&Handler{
					OnBinary: func(conn *Conn, msg []byte) {
						conn.Write(msg)
					},
				}, WithReadDeadline(time.Hour), WithReadBufferSize(bufSize))

				port, address := nexttestport()

				go func() {
					s.Serve(port)
				}()

				time.Sleep(100 * time.Millisecond)
				defer s.Close()

				payload := make([]byte, size)
				rand.Read(payload)

				mcopy := make([]byte, len(payload))
				copy(mcopy, payload)

				h := header{
					OpCode: opBinary,
					Fin:    true,
					Masked: true,
					Length: int64(len(payload)),
				}

				cipher(mcopy, h.Mask, 0)

				codec := newCodec()
				hdBytes, err := codec.BuildHeader(h)
				if err != nil {
					b.Fatal(err)
				}

				fullMsg := append(hdBytes, mcopy...)

				b.ResetTimer()
				b.ReportAllocs()
				b.SetBytes(int64(size))

				b.RunParallel(func(pb *testing.PB) {
					conn, rb := benchconn(b, address)
					defer conn.Close()

					buf, err := newCircbuf(16384)
					if err != nil {
						b.Fatal(err)
					}

					for i := 0; i < batchSize; i++ {
						_, err := conn.Write(fullMsg)
						if err != nil {
							b.Fatal(err)
						}
					}

					for pb.Next() {
						var rh header

						for {
							err = codec.ReadHeader(&rh, buf)
							if err == nil {
								break
							} else if err == ErrDataNeeded {
								n, err := rb.Read(buf.next())
								if err != nil {
									b.Fatal(err)
								}
								buf.advanceWrite(n)
							} else {
								b.Fatal(err)
							}
						}

						if buf.buffered() < int(rh.Length) {
							needed := int(rh.Length) - buf.buffered()
							n, err := io.ReadFull(rb, buf.next()[:needed])
							if err != nil {
								b.Fatal(err)
							}
							buf.advanceWrite(n)
						}

						buf.advanceRead(int(rh.Length))

						_, err = conn.Write(fullMsg)
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			})
		}
	}
}
