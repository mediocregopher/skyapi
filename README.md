# skyapi

A simple websocket API which sits in front of SkyDNS/etcd and manages active
processes for services.

Processes make a long-held websocket connection to skyapi with information about
the service they're providing for, their host/port, and optionally weight or
priority (See the [SkyDNS docs](https://github.com/skynetservices/skydns) for
more on weight/priority). When the connection is closed the entry for that
process will be removed from its service.

## DNS Entries

SkyDNS always has a root domain it serves, and all of its entries must be under
this domain. The root domain is configurable, but skyapi must also know about
it. For the rest of the examples we're going to say the root domain is
`turtles.com`.

skyapi adds service entries to the `services` sub-domain. For example, if a
process creates a connection saying it provides the `foo` service then it will
be added to the pool under `foo.services.turtle.com`.

## Build

    go get github.com/mediocregopher/skyapi
    cd $GOPATH/src/github.com/mediocregopher/skyapi
    go build

## Usage

skyapi listens by default on port 8053. It exposes a single websocket endpoint,
`/provide`. This endpoint accepts the following GET parameters:

* **service** (Required) - The name of the service being provided
* **host** - The host/ip the process can be reached on. Defaults to the ip the
  request is coming from
* **port** (Required) - The port the process can be reached on
* **priority** - The priority the process should be given in its pool. Default 1.
* **weight** - The weight the process should be given in its pool. Default 100.

For example: `GET /provide?service=foo&host=127.0.0.1&port=9999&priority=1`

Once the websocket is created a corresponding DNS entry will be created.

The client *should* periodically send websocket pings to the server, and the
server will also send pings periodically. If the server's pings to the client
fail it will immediately remove the client's entry from the service pool it was
providing for.
