package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"code.dogecoin.org/gossip/dnet"

	"code.dogecoin.org/governor"

	"code.dogecoin.org/dogemap-backend/internal/collector"
	"code.dogecoin.org/dogemap-backend/internal/geoip"
	"code.dogecoin.org/dogemap-backend/internal/store"
	"code.dogecoin.org/dogemap-backend/internal/web"
)

const WebAPIDefaultPort = 8091
const DogeNetDefaultPort = 8085
const IdentityDefaultPort = 8099
const CoreNodeDefaultPort = 22556
const DBFile = "dogemap.db"
const GeoIPFile = "dbip-city-ipv4-num.csv"
const DefaultStorage = "./storage"
const DefaultWebDir = "./web"

var stderr = log.New(os.Stderr, "", 0)

func main() {
	var crawl int
	binds := []dnet.Address{}
	core := dnet.Address{}
	dbfile := DBFile
	webdir := DefaultWebDir
	dogenetAddr := ""
	identityAddr := ""
	dir := DefaultStorage
	flag.Func("dir", "<path> - storage directory (default './storage')", func(arg string) error {
		ent, err := os.Stat(arg)
		if err != nil {
			stderr.Fatalf("--dir: %v", err)
		}
		if !ent.IsDir() {
			stderr.Fatalf("--dir: not a directory: %v", arg)
		}
		dir = arg
		return nil
	})
	flag.Func("web", "<path> - web directory (default './web')", func(arg string) error {
		ent, err := os.Stat(arg)
		if err != nil {
			stderr.Fatalf("--web: %v", err)
		}
		if !ent.IsDir() {
			stderr.Fatalf("--web: not a directory: %v", arg)
		}
		webdir = arg
		return nil
	})
	flag.IntVar(&crawl, "crawl", 0, "number of core node crawlers")
	flag.StringVar(&dbfile, "db", DBFile, "path to SQLite database (relative: in storage dir)")
	flag.Func("bind", "Bind web API <ip>:<port> (use [<ip>]:<port> for IPv6)", func(arg string) error {
		addr, err := parseIPPort(arg, "bind", WebAPIDefaultPort)
		if err != nil {
			return err
		}
		binds = append(binds, addr)
		return nil
	})
	flag.Func("core", "<ip>:<port> (use [<ip>]:<port> for IPv6)", func(arg string) error {
		addr, err := parseIPPort(arg, "core", CoreNodeDefaultPort)
		if err != nil {
			return err
		}
		core = addr
		return nil
	})
	flag.Func("dogenet", "<ip>:<port> (use [<ip>]:<port> for IPv6)", func(arg string) error {
		addr, err := parseIPPort(arg, "dogenet", DogeNetDefaultPort)
		if err != nil {
			return err
		}
		dogenetAddr = addr.String()
		return nil
	})
	flag.Func("identity", "<ip>:<port> (use [<ip>]:<port> for IPv6)", func(arg string) error {
		addr, err := parseIPPort(arg, "identity", IdentityDefaultPort)
		if err != nil {
			return err
		}
		identityAddr = addr.String()
		return nil
	})
	flag.Parse()
	if flag.NArg() > 0 {
		log.Printf("Unexpected argument: %v", flag.Arg(0))
		os.Exit(1)
	}
	if len(binds) < 1 {
		binds = append(binds, dnet.Address{
			Host: net.IP([]byte{0, 0, 0, 0}),
			Port: WebAPIDefaultPort,
		})
	}

	// get the private key from the KEY env-var
	nodeKey := keysFromEnv()
	log.Printf("Node PubKey is: %v", hex.EncodeToString(nodeKey.Pub[:]))

	// open database.
	dbpath := path.Join(dir, dbfile)
	db, err := store.NewSQLiteStore(dbpath, context.Background())
	if err != nil {
		log.Printf("Error opening database: %v [%s]\n", err, dbpath)
		os.Exit(1)
	}

	gov := governor.New().CatchSignals().Restart(1 * time.Second)

	// stay connected to local node if specified.
	if core.IsValid() {
		gov.Add("local-node", collector.New(db, core, 60*time.Second, true))
	}

	// start crawling Core Nodes.
	for n := 0; n < crawl; n++ {
		gov.Add(fmt.Sprintf("crawler-%d", n), collector.New(db, store.Address{}, 5*time.Minute, false))
	}

	// load the geoIP database
	// https://github.com/sapics/ip-location-db/tree/main/dbip-city/dbip-city-ipv4-num.csv.gz
	geoFile := path.Join(dir, GeoIPFile)
	log.Printf("loading GeoIP database: %v", geoFile)
	geoIP, err := geoip.NewGeoIPDatabase(geoFile)
	if err != nil {
		log.Printf("Error reading GeoIP database: %v [%s]\n", err, geoFile)
		os.Exit(1)
	}

	// start the web server.
	for _, to := range binds {
		gov.Add("web-api", web.New(to, db, geoIP, webdir, dogenetAddr, identityAddr))
	}

	// start the store trimmer
	gov.Add("store", store.NewStoreTrimmer(db))

	// run services until interrupted.
	gov.Start()
	gov.WaitForShutdown()
	fmt.Println("finished.")
}

// Parse an IPv4 or IPv6 address with optional port.
func parseIPPort(arg string, name string, defaultPort uint16) (dnet.Address, error) {
	// net.SplitHostPort doesn't return a specific error code,
	// so we need to detect if the port it present manually.
	colon := strings.LastIndex(arg, ":")
	bracket := strings.LastIndex(arg, "]")
	if colon == -1 || (arg[0] == '[' && bracket != -1 && colon < bracket) {
		ip := net.ParseIP(arg)
		if ip == nil {
			return dnet.Address{}, fmt.Errorf("bad --%v: invalid IP address: %v (use [<ip>]:port for IPv6)", name, arg)
		}
		return dnet.Address{
			Host: ip,
			Port: defaultPort,
		}, nil
	}
	res, err := dnet.ParseAddress(arg)
	if err != nil {
		return dnet.Address{}, fmt.Errorf("bad --%v: invalid IP address: %v (use [<ip>]:port for IPv6)", name, arg)
	}
	return res, nil
}

func keysFromEnv() dnet.KeyPair {
	// get the private key from the KEY env-var
	nodeHex := os.Getenv("KEY")
	os.Setenv("KEY", "") // don't leave the key in the environment
	if nodeHex == "" {
		log.Printf("Missing KEY env-var: node public-private keypair (32 bytes; see `dogenet genkey`)")
		os.Exit(3)
	}
	nodeKey, err := hex.DecodeString(nodeHex)
	if err != nil {
		log.Printf("Invalid KEY hex in env-var: %v", err)
		os.Exit(3)
	}
	if len(nodeKey) != 32 {
		log.Printf("Invalid KEY hex in env-var: must be 32 bytes")
		os.Exit(3)
	}
	return dnet.KeyPairFromPrivKey((*[32]byte)(nodeKey))
}
