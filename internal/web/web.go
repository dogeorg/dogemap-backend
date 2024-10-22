package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"code.dogecoin.org/dogemap-backend/internal/geoip"
	"code.dogecoin.org/dogemap-backend/internal/spec"
	"code.dogecoin.org/gossip/dnet"
	"code.dogecoin.org/governor"
)

func New(bind spec.Address, store spec.Store, geoIP *geoip.GeoIPDatabase, webdir string, dogeNetAddr string, identityAddr string) governor.Service {
	mux := http.NewServeMux()
	a := &WebAPI{
		_store: store,
		srv: http.Server{
			Addr:    bind.String(),
			Handler: mux,
		},
		geoIP: geoIP,
	}
	if dogeNetAddr != "" {
		// used by /nodes API
		a.dogeNetUrl = fmt.Sprintf("http://%v/nodes", dogeNetAddr)
	}
	if identityAddr != "" {
		// used by /nodes API
		a.locationsUrl = fmt.Sprintf("http://%v/locations", identityAddr)
		// create a proxy for /chits API
		identityUrl := &url.URL{Scheme: "http", Host: identityAddr, Path: "/"}
		a.identityProxy = httputil.NewSingleHostReverseProxy(identityUrl)
	}

	mux.HandleFunc("/nodes", a.getNodes)
	mux.HandleFunc("/chits", a.getChits)

	fs := http.FileServer(http.Dir(webdir))
	mux.Handle("/", fs)

	return a
}

type WebAPI struct {
	governor.ServiceCtx
	_store        spec.Store
	store         spec.Store
	srv           http.Server
	geoIP         *geoip.GeoIPDatabase
	dogeNetUrl    string
	locationsUrl  string
	identityProxy *httputil.ReverseProxy
}

// called on any
func (a *WebAPI) Stop() {
	// new goroutine because Shutdown() blocks
	go func() {
		// cannot use ServiceCtx here because it's already cancelled
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a.srv.Shutdown(ctx) // blocking call
		cancel()
	}()
}

// goroutine
func (a *WebAPI) Run() {
	a.store = a._store.WithCtx(a.Context) // Service Context is first available here
	log.Printf("HTTP server listening on: %v\n", a.srv.Addr)
	if err := a.srv.ListenAndServe(); err != http.ErrServerClosed { // blocking call
		log.Printf("HTTP server: %v\n", err)
	}
}

type MapNode struct {
	SubVer   string  `json:"subver"` // IP address
	Lat      string  `json:"lat"`
	Lon      string  `json:"lon"`
	City     string  `json:"city"`
	Country  string  `json:"country"`
	IPInfo   *string `json:"ipinfo"`   // always null
	Identity string  `json:"identity"` // can be empty
	Core     bool    `json:"core"`     // true if core node
}

type GetChit struct {
	Identity string `json:"identity"` // identity (node owner) pubkey hex
	Node     string `json:"node"`     // node pubkey hex
}

type IdentChit struct {
	Lat     string `json:"lat"`     // WGS84 +/- 90 degrees, 60 seconds (accurate to 1850m)
	Lon     string `json:"lon"`     // WGS84 +/- 180 degrees, 60 seconds (accurate to 1850m)
	Country string `json:"country"` // [2] ISO 3166-1 alpha-2 code (optional)
	City    string `json:"city"`    // [30] city name (optional)
}

func (a *WebAPI) getChits(w http.ResponseWriter, r *http.Request) {
	options := "POST, OPTIONS"
	if r.Method == http.MethodPost {
		if a.identityProxy != nil {
			// proxy /chits through to `identity` service, which returns
			// an object containing Profiles keyed on identity-hex.
			// if an identity is not found, it is omitted from the result.
			a.identityProxy.ServeHTTP(w, r)
		} else {
			chits := make(map[string]any) // empty object
			sendJson(w, chits, options)
		}
	} else {
		sendOptions(w, r, options)
	}
}

func (a *WebAPI) getNodes(w http.ResponseWriter, r *http.Request) {
	options := "GET, OPTIONS"
	if r.Method == http.MethodGet {
		coreNodes, err := a.store.NodeList()
		if err != nil {
			http.Error(w, fmt.Sprintf("error in query: %s", err.Error()), http.StatusInternalServerError)
			return
		}

		// unique nodes (may find both a dogenet and a core node)
		nodeMap := make(map[string]MapNode, 8192)

		// fetch all nodes from `dogenet` and their identities
		netNodes := make([]spec.NetNode, 0, 8192)
		identities := make(map[string]IdentChit, 8192)
		if a.dogeNetUrl != "" {
			err = fetchJson(a.dogeNetUrl, nil, &netNodes)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// all identities from the set of nodes
			if a.locationsUrl != "" {
				var getChits []GetChit
				for _, net := range netNodes {
					if net.Identity != "" {
						getChits = append(getChits, GetChit{
							Identity: net.Identity,
							Node:     net.PubKey,
						})
					}
				}

				// fetch identities from `identity` handler
				err = fetchJson(a.locationsUrl, getChits, &identities)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}

			// make result nodes for all dogenet nodes
			for _, node := range netNodes {
				if profile, found := identities[node.Identity]; found {
					nodeMap[node.Address] = MapNode{
						SubVer:   node.Address,
						Lat:      profile.Lat,
						Lon:      profile.Lon,
						Country:  profile.Country,
						City:     profile.City,
						IPInfo:   nil,
						Identity: node.Identity,
					}
				} else {
					// note: NetNode.Address is already normalized.
					// BUG: dnet.ParseAddress (net.ParseIP) always returns IPv6.
					addr, err := dnet.ParseAddress(node.Address)
					if err != nil {
						log.Printf("[GET /nodes] invalid core address: %v", node.Address)
						continue
					}
					// re-normalize to IPv4 (see above)
					addr = normalizeIP4(addr)
					lat, lon, country, city := a.geoIP.FindLocation(addr.Host)
					nodeMap[node.Address] = MapNode{
						SubVer:   node.Address,
						Lat:      lat,
						Lon:      lon,
						Country:  country,
						City:     city,
						IPInfo:   nil,
						Identity: node.Identity,
					}
				}
			}
		}

		// add core nodes to the result.
		for _, core := range coreNodes {
			addr, err := dnet.ParseAddress(core.Address)
			if err != nil {
				log.Printf("[GET /nodes] invalid core address: %v", core.Address)
				continue
			}
			addr = normalizeIP4(addr)
			key := addr.String() // normalized address
			if _, found := nodeMap[key]; !found {
				lat, lon, country, city := a.geoIP.FindLocation(addr.Host)
				nodeMap[key] = MapNode{
					SubVer:   key,
					Lat:      lat,
					Lon:      lon,
					Country:  country,
					City:     city,
					IPInfo:   nil,
					Identity: "",
					Core:     true,
				}
			}
		}

		// values from the map
		nodes := make([]MapNode, 0, len(nodeMap))
		for _, node := range nodeMap {
			nodes = append(nodes, node)
		}

		sendJson(w, nodes, options)
	} else {
		sendOptions(w, r, options)
	}
}

// normalizeIP4 normalizes an Address to IPv4 if possible.
func normalizeIP4(addr spec.Address) spec.Address {
	ipv4 := addr.Host.To4()
	if ipv4 != nil {
		return spec.Address{Host: ipv4, Port: addr.Port}
	}
	return addr
}
