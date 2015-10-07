package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-etcd/etcd"
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
	etcdClient      *etcd.Client
	etcdClientLock  sync.Mutex
)

func main() {
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

	etcdClientLock.Lock()
	etcdClient = etcd.NewClient(etcdAPIs)
	etcdClientLock.Unlock()

	http.Handle("/provide", http.HandlerFunc(handler))

	kv := llog.KV{"addr": listenAddr}
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
	id                string
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
	file := path.Join(dir, cd.id)
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

	defer llog.Warn("closed", kv)
	defer etcdDelete(cd)
	llog.Info("connected", kv)

	tick := time.Tick(timeout / 2)
	if !doTick(conn, cd, kv) {
		return
	}
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

func parseConnData(r *http.Request) (connData, error) {
	service := r.FormValue("service")
	category := r.FormValue("category")
	host := r.FormValue("host")
	portStr := r.FormValue("port")
	var port int
	var err error

	if service == "" {
		err = fmt.Errorf("service and port are required parameters")
		return connData{}, err
	}

	if category == "" {
		category = defaultCategory
	}

	if host == "" {
		host = r.RemoteAddr[:strings.Index(r.RemoteAddr, ":")]
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

	return connData{
		id:       id,
		service:  service,
		category: category,
		Host:     host,
		Port:     port,
		Priority: priority,
		Weight:   weight,
	}, nil
}

// Creates the given dir (and all of its parent directories if they don't
// already exist). Will not return an error if the given directory already
// exists
func MkDirP(ec *etcd.Client, dir string) error {
	parts := make([]string, 0, 4)
	for {
		parts = append(parts, dir)
		dir = path.Dir(dir)
		if dir == "/" {
			break
		}
	}

	for i := range parts {
		ai := len(parts) - i - 1
		_, err := ec.CreateDir(parts[ai], 0)
		if err != nil && err.(*etcd.EtcdError).ErrorCode != 105 {
			return err
		}
	}
	return nil
}

func etcdStore(cd connData) error {
	etcdClientLock.Lock()
	defer etcdClientLock.Unlock()

	dir, file := cd.toPath()
	if err := MkDirP(etcdClient, dir); err != nil {
		return err
	}

	j, err := json.Marshal(cd)
	if err != nil {
		return err
	}

	_, err = etcdClient.Set(file, string(j), uint64(timeout.Seconds()))
	if err != nil {
		return err
	}

	return nil
}

func etcdDelete(cd connData) error {
	_, file := cd.toPath()

	etcdClientLock.Lock()
	_, err := etcdClient.Delete(file, false)
	etcdClientLock.Unlock()

	return err
}
