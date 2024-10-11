# DogeMap Backend

The DogeMap backend serves the [dogemap-ui](https://github.com/dogeorg/dogemap-ui)
web frontend on port 8091.

DogeMap shows all the Dogeocoin Nodes on a world map – with limited accuracy.

It is intended to give a general feel for how Dogecoin nodes are distributed
across the world.

DogeBox owners can publish an Identity Profile that specifies a location of
their choosing on the map.

In the future, we will be adding ways to view public profiles and search for
nodes offering services to the public, e.g. shops.

## Web API

DogeMap backend also serves an API on the same port, which is used by the DogeMap.

```
GET /nodes

[{"subver":"1.2.3.4:22556","lat":"40.7","lon":"-73.9","city":"New York","country":"US","ipinfo":null,"identity":"","core":true}, ...]
```

## Core Nodes

When DogeMap Backend is configured with a local Core Node address, it
*scrapes* known Core Nodes periodically from the local Core Node.
This is sufficient to approximately map out the active Core Nodes
over time, without placing any additional load on the Core network.

## Identity Profiles

When DogeMap Backend is connected to the [identity](https://github.com/dogeorg/identity)
pup, it looks up the latitude and longitude published by the owner of the DogeBox and
uses that location on the DogeMap.

If a given DogeBox chooses not to announce an identity profile, the
DogeMap will use Geo IP lookup based on the DogeBox public IP address.

## Geo IP

The DogeMap Backend uses the free DB-IP "IP to City Lite" database from:
https://github.com/sapics/ip-location-db/tree/main/dbip-city/

This database is licensed under CC BY 4.0 from DB-IP.com.

For Core Nodes, the node's IP address is always looked up in this database.
For DogeNet nodes, we use the node's Identity Profile if one is available,
otherwise we use this Geo IP database.

Note that this database has limited accuracy. There will be occasional
incorrect results, and some IP addresses will not be found at all.
IP allocations also change over time.
