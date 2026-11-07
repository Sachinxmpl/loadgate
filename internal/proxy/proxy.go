package proxy

import (
	"io"
	"net"
	"sync"
	"time"
)

type closeWriter interface {
	CloseWrite() error
}

// tells c's peer -> no more bytes from this side without disturbing  peer's reply still n flight
// if c cannot half-close, it is fully closed
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

// Relays both ways between client and backend until both directions end, then close both socket at once
// When one-direction ends, its EOF is forwarded to the other side as a half-close, other open direction is given drainTimeout to finish
func L4(client, backend net.Conn, drainTimeout time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)

	var once sync.Once

	copyOneWay := func(dst, src net.Conn) {
		defer wg.Done()

		_, _ = io.Copy(dst, src)
		halfCloseWrite(dst)

		// first direction to finish sets readline for other
		once.Do(func() {
			// Other direction reads from dst and writes to src
			// both bounded
			d := time.Now().Add(drainTimeout)
			dst.SetReadDeadline(d)
			src.SetWriteDeadline(d)
		})
	}

	// client -> backend
	// exists when: client sends EOF/errors, or (as the losign half) its read deadline set by the other gorouting fires
	go copyOneWay(backend, client)

	//backend -> client
	//exists when: backend sends EOF/erros, or its read deadline fires
	go copyOneWay(client, backend)

	wg.Wait()

	client.Close()
	backend.Close()
}
