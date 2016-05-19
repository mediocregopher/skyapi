package client

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/levenlabs/go-srvclient"
)

// Opts represent a set of options which can be passed into ProvideOpts. Some
// are required to be filled in and are marked as such. The rest have defaults
// listed in their descriptions
type Opts struct {

	// Required. The address of the skyapi instance to connect to. Should be
	// "host:port" or a hostname that can be resolved via SRV lookup
	SkyAPIAddr string

	// Required. The name of the service this process is providing for
	Service string

	// Required. The address to advertise this process at. Can be either
	// "host:port" or ":port"
	ThisAddr string

	// Optional. A prefix that will be prepended to the hashed id that is created
	// for the domain. Useful for passing grouping information such as datacenter.
	Prefix string

	// Optional. The category this service falls under. Defaults to the skyapi
	// server's global default, usually "services"
	Category string

	// Optional. The priority and weight values which will be stored along with
	// this entry, and which will be returned in SRV requests to the skyapi
	// server. Defaults to 1 and 100, respectively
	Priority, Weight int

	// Optional. Setting to a positive number will cause the connection to
	// attempt to be remade up to that number of times on a disconnect. After
	// that many failed attempts an error is returned. If 0 an error is returned
	// on the first disconnect. If set to a negative number reconnect attempts
	// will continue forever
	ReconnectAttempts int

	// Optional. The interval to ping the server at in order to ensure the
	// connection is still alive. Defaults to 10 seconds.
	Interval time.Duration

	// Optional. If set, can be later closed by another process to indicate that
	// the client should stop advertising and close. A nil error will be
	// returned by the ProvideOpts method in this case.
	StopCh chan struct{}
}

func (o Opts) stopped() bool {
	select {
	case _, stopped := <-o.StopCh:
		return stopped
	default:
		return false
	}
}

// Provide is a DEPRECATED method for making a connection to a skyapi instance.
// Use ProvideOpts instead
func Provide(
	addr, service, thisAddr string, priority, weight, reconnectAttempts int,
	interval time.Duration,
) error {
	o := Opts{
		SkyAPIAddr:        addr,
		Service:           service,
		ThisAddr:          thisAddr,
		Priority:          priority,
		Weight:            weight,
		ReconnectAttempts: reconnectAttempts,
		Interval:          interval,
	}
	return provide(o)
}

// ProvideOpts uses the given Opts value to connect to a skyapi instance and
// declare a service being provided for. It blocks until disconnect or some
// other error.
func ProvideOpts(o Opts) error {
	if o.Priority == 0 {
		o.Priority = 1
	}
	if o.Weight == 0 {
		o.Weight = 100
	}
	if o.Interval == 0 {
		o.Interval = 10 * time.Second
	}
	return provide(o)
}

func provide(o Opts) error {
	host, port, err := net.SplitHostPort(o.ThisAddr)
	if err != nil {
		return err
	}

	u, err := url.Parse("ws://" + o.SkyAPIAddr + "/provide")
	if err != nil {
		return err
	}
	vals := url.Values{}
	vals.Set("service", o.Service)
	if host != "" {
		vals.Set("host", host)
	}
	if port != "" {
		vals.Set("port", port)
	}
	if o.Category != "" {
		vals.Set("category", o.Category)
	}
	if o.Prefix != "" {
		vals.Set("prefix", o.Prefix)
	}
	vals.Set("priority", strconv.Itoa(o.Priority))
	vals.Set("weight", strconv.Itoa(o.Weight))
	u.RawQuery = vals.Encode()

	tries := 0
	for {
		tries++

		didSucceed, err := innerProvide(o.SkyAPIAddr, u, o.Interval, o.StopCh)
		if o.stopped() {
			return nil
		} else if didSucceed {
			tries = 0
		}
		if o.ReconnectAttempts >= 0 && tries >= o.ReconnectAttempts {
			return err
		}
		time.Sleep(1 * time.Second)
	}
}

func innerProvide(addr string, u *url.URL, interval time.Duration, stopCh chan struct{}) (
	bool, error,
) {
	var didSucceed bool

	addr = srvclient.MaybeSRV(addr)

	rawConn, err := net.Dial("tcp", addr)
	if err != nil {
		return didSucceed, err
	}
	defer rawConn.Close()

	u.Host = addr
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

	for {
		select {
		case <-tick:
			if err := doTick(conn, addr, interval); err != nil {
				return didSucceed, fmt.Errorf("connection to %s closed: %s", addr, err)
			}
			didSucceed = true

		case <-closeCh:
			return didSucceed, fmt.Errorf("connection to %s closed", addr)

		case <-stopCh:
			conn.Close()
			return didSucceed, nil
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
