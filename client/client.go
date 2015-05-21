package client

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Provide makes a connection to the skyapi instance at the given address and
// informs it that this process is providing for the given service, and that it
// should use the given address/priority/weight information for the DNS
// entry. It will ping the server at the given interval to make sure the
// connection is still active. If the server disconnects Provide will attempt to
// reconnect before returning an error, unless reconnectAttempts is 0. If
// reconnectAttemps is -1 Provide will attempt to reconnect forever.
func Provide(
	addr, service, thisAddr string, priority, weight, reconnectAttempts int,
	interval time.Duration,
) error {
	parts := strings.Split(thisAddr, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid addr %q", thisAddr)
	}

	u, err := url.Parse("ws://" + addr + "/provide")
	if err != nil {
		return err
	}
	vals := url.Values{}
	vals.Set("service", service)
	if parts[0] != "" {
		vals.Set("host", parts[0])
	}
	vals.Set("port", parts[1])
	vals.Set("priority", strconv.Itoa(priority))
	vals.Set("weight", strconv.Itoa(weight))
	u.RawQuery = vals.Encode()

	tries := 0
	for {
		tries++

		didSucceed, err := innerProvide(addr, u, interval)
		if didSucceed {
			tries = 0
		}
		if reconnectAttempts >= 0 && tries >= reconnectAttempts {
			return err
		}
		time.Sleep(1 * time.Second)
	}
}

func innerProvide(
	addr string, u *url.URL,
	interval time.Duration,
) (
	bool, error,
) {
	var didSucceed bool

	rawConn, err := net.Dial("tcp", addr)
	if err != nil {
		return didSucceed, err
	}
	defer rawConn.Close()

	conn, _, err := websocket.NewClient(rawConn, u, nil, 0, 0)
	if err != nil {
		return didSucceed, err
	}

	closeCh := make(chan struct{})
	go readDiscard(conn, closeCh)
	tick := time.Tick(interval)

	if err := doTick(conn, addr, interval); err != nil {
		return didSucceed, fmt.Errorf("connection to %s closed: %s", addr, err)
	}
	didSucceed = true

	for {
		select {
		case <-tick:
			if err := doTick(conn, addr, interval); err != nil {
				return didSucceed, fmt.Errorf("connection to %s closed: %s", addr, err)
			}

		case <-closeCh:
			return didSucceed, fmt.Errorf("connection to %s closed", addr)
		}
	}
}

func doTick(conn *websocket.Conn, addr string, interval time.Duration) error {
	deadline := time.Now().Add(interval / 2)
	err := conn.WriteControl(websocket.PingMessage, nil, deadline)
	if err != nil {
		return err
	}
	return nil
}

func readDiscard(conn *websocket.Conn, closeCh chan struct{}) {
	for {
		if _, _, err := conn.NextReader(); err != nil {
			close(closeCh)
			return
		}
	}
}
