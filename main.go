package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/coreos/etcd/client"
	"github.com/gorilla/websocket"
	"github.com/levenlabs/go-llog"
	"github.com/levenlabs/golib/rpcutil"
	"github.com/mediocregopher/lever"
)

var (
	listenAddr      string
	etcdAPIs        []string
	dnsRoot         string
	defaultCategory string
	timeout         time.Duration
	etcdKeysAPI     client.KeysAPI
	etcdCtx         = context.Background()
)

func main() {
	rand.Seed(time.Now().UnixNano())

	l := lever.New("skyapi", nil)
	l.Add(lever.Param{
		Name:        "--listen-addr",
		Description: "Address to listen for websocket connections on",
		Default:     ":8053",
	})
	l.Add(lever.Param{
		Name:         "--etcd-api",
		Description:  "scheme://address an etcd node in the cluster can be found on. Can be specified multiple times",
		DefaultMulti: []string{"http://127.0.0.1:4001"},
	})
	l.Add(lever.Param{
		Name:        "--etcd-pool-size",
		Description: "deprecated option, now ignored",
	})
	l.Add(lever.Param{
		Name:        "--dns-root",
		Description: "Root domain that SkyDNS is serving on",
		Default:     "skydns.local.",
	})
	l.Add(lever.Param{
		Name:        "--timeout",
		Description: "The TTL for entries in SkyDNS, in seconds. The server will ping at half this value, the client should also",
		Default:     "30",
	})
	l.Add(lever.Param{
		Name:        "--default-category",
		Description: "The default category to file incoming connections over, if they don't specify",
		Default:     "services",
	})
	l.Add(lever.Param{
		Name:        "--log-level",
		Description: "Adjust the log level. Valid options are: error, warn, info, debug",
		Default:     "info",
	})
	l.Parse()

	listenAddr, _ = l.ParamStr("--listen-addr")
	etcdAPIs, _ = l.ParamStrs("--etcd-api")
	dnsRoot, _ = l.ParamStr("--dns-root")
	timeoutSecs, _ := l.ParamInt("--timeout")
	timeout = time.Duration(timeoutSecs) * time.Second
	defaultCategory, _ = l.ParamStr("--default-category")
	logLevel, _ := l.ParamStr("--log-level")
	llog.SetLevelFromString(logLevel)

	kv := llog.KV{"addrs": etcdAPIs}
	llog.Info("creating etcd client", kv)
	etcdClient, err := client.New(client.Config{
		Endpoints: etcdAPIs,
	})
	if err != nil {
		kv["err"] = err
		llog.Fatal("error creating etcd client", kv)
	}
	// TODO the etcd client docs are pretty bad and don't adequetly enumerate
	// when this is needed and when it isn't, so we may need this afterall
	//go func() {
	//	for {
	//		if err := etcdClient.AutoSync(etcdCtx, 10*time.Second); err != nil {
	//			llog.Error("failed syncing etcd cluster", llog.KV{
	//				"err":     err,
	//				"members": etcdClient.Endpoints(),
	//			})
	//		}
	//	}
	//}()
	etcdKeysAPI = client.NewKeysAPI(etcdClient)

	http.Handle("/provide", http.HandlerFunc(handler))

	kv = llog.KV{"addr": listenAddr}
	llog.Info("listening for websocket connections", kv)
	kv["err"] = http.ListenAndServe(listenAddr, nil)
	llog.Fatal("failed listening for websocket connections", kv)
}

var upgrader = websocket.Upgrader{
	// Buffer sizes are 0 because nothing should ever be read or written
	ReadBufferSize:  0,
	WriteBufferSize: 0,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type connData struct {
	prefix, id        string
	service, category string
	Host              string `json:"host"`
	Port              int    `json:"port,omitempty"`
	Priority          int    `json:"priority,omitempty"`
	Weight            int    `json:"weight,omitempty"`
}

func (cd connData) toPath() (string, string) {
	parts := strings.Split(dnsRoot, ".")
	partsR := append(make([]string, 0, len(parts)+2), "/skydns")
	for i := len(parts) - 1; i >= 0; i-- {
		partsR = append(partsR, parts[i])
	}
	partsR = append(partsR, cd.category, cd.service)
	dir := path.Join(partsR...)
	key := cd.id
	if cd.prefix != "" {
		key = cd.prefix + "-" + cd.id
	}
	file := path.Join(dir, key)
	return dir, file
}

func errorMessagef(conn *websocket.Conn, s string, args ...interface{}) {
	msg := fmt.Sprintf("ERROR: "+s, args...)
	conn.WriteMessage(websocket.TextMessage, []byte(msg))
}

func handler(w http.ResponseWriter, r *http.Request) {
	kv := rpcutil.RequestKV(r)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		kv["err"] = err
		llog.Warn("could not upgrade to websocket", kv)
		return
	}
	closeCh := make(chan struct{})
	go readDiscard(conn, closeCh)
	defer conn.Close()

	cd, err := parseConnData(r)
	if err != nil {
		kv["err"] = err
		llog.Warn("could not parse conn data", kv)
		errorMessagef(conn, "parseConnData: %s", err)
		return
	}

	kv["id"] = cd.id
	kv["service"] = cd.service
	kv["category"] = cd.category
	kv["addr"] = net.JoinHostPort(cd.Host, strconv.Itoa(cd.Port))

	defer llog.Info("closed", kv)
	defer func() {
		etcdDelete(cd)
	}()
	llog.Info("connected", kv)

	if !doTick(conn, cd, kv) {
		return
	}

	// sleep a random amount within the timeout before starting the loop. this
	// is to prevent a repeated stampede, if skyapi restarts and all of the
	// services reconnect, without this all of their ticks would be going off at
	// the same time
	time.Sleep(time.Duration(rand.Int63n(int64(timeout) * 3 / 4)))

	tick := time.Tick(timeout / 2)
	for {
		select {
		case <-tick:
			if !doTick(conn, cd, kv) {
				return
			}
		case <-closeCh:
			return
		}
	}
}

func doTick(conn *websocket.Conn, cd connData, kv llog.KV) bool {
	deadline := time.Now().Add(timeout / 2)
	err := conn.WriteControl(websocket.PingMessage, nil, deadline)
	if err != nil {
		llog.Warn("timedout", kv)
		return false
	}

	if err = etcdStore(cd); err != nil {
		kv["err"] = err
		llog.Error("storing etcd data", kv)
		delete(kv, "err") // the kv gets used again later, so delete err
		errorMessagef(conn, "storing etcd data: %s", err)
		return false
	}
	return true
}

func readDiscard(conn *websocket.Conn, closeCh chan struct{}) {
	for {
		if _, _, err := conn.NextReader(); err != nil {
			close(closeCh)
			return
		}
	}
}

var (
	zeroIP4 = net.ParseIP("0.0.0.0")
	zeroIP6 = net.ParseIP("::")
)

func parseConnData(r *http.Request) (connData, error) {
	service := r.FormValue("service")
	category := r.FormValue("category")
	host := r.FormValue("host")
	portStr := r.FormValue("port")
	prefix := r.FormValue("prefix")
	var port int
	var err error

	if service == "" {
		err = fmt.Errorf("service and port are required parameters")
		return connData{}, err
	}

	if category == "" {
		category = defaultCategory
	}

	if host != "" {
		if hostIP := net.ParseIP(host); hostIP != nil {
			if hostIP.Equal(zeroIP4) || hostIP.Equal(zeroIP6) {
				host = ""
			}
		}
	}
	if host == "" {
		if host, _, err = net.SplitHostPort(r.RemoteAddr); err != nil {
			return connData{}, err
		}
	}

	if portStr != "" {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return connData{}, err
		}
	}

	var priority int
	priorityStr, weightStr := r.FormValue("priority"), r.FormValue("weight")
	if priorityStr != "" {
		if priority, err = strconv.Atoi(priorityStr); err != nil {
			return connData{}, err
		}
	}

	var weight int
	if weightStr != "" {
		if weight, err = strconv.Atoi(weightStr); err != nil {
			return connData{}, err
		}
	}

	sha := sha1.New()
	fmt.Fprint(sha, service)
	fmt.Fprint(sha, category)
	fmt.Fprint(sha, host)
	fmt.Fprint(sha, port)
	id := hex.EncodeToString(sha.Sum(nil))
	if prefix != "" {
		id = id[:20]
	}

	return connData{
		prefix:   prefix,
		id:       id,
		service:  service,
		category: category,
		Host:     host,
		Port:     port,
		Priority: priority,
		Weight:   weight,
	}, nil
}

func etcdStore(cd connData) error {
	_, file := cd.toPath()

	j, err := json.Marshal(cd)
	if err != nil {
		return err
	}

	_, err = etcdKeysAPI.Set(etcdCtx, file, string(j), &client.SetOptions{
		TTL: timeout,
	})
	if err != nil {
		return err
	}

	return nil
}

func etcdDelete(cd connData) error {
	_, file := cd.toPath()
	_, err := etcdKeysAPI.Delete(etcdCtx, file, nil)
	return err
}
